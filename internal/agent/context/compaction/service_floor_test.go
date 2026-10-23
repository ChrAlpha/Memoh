package compaction

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// floorRows are rows created after a floor that ends on 2026-10-20 17:42 UTC.
func floorRows(t *testing.T, cfg TriggerConfig) []sqlc.ListUncompactedMessagesBySessionRow {
	t.Helper()
	rows := fusionQualityRows(t, cfg)
	for i := range rows {
		rows[i].CreatedAt = pgtype.Timestamptz{Time: time.Date(2026, 10, 20, 18, i, 0, 0, time.UTC), Valid: true}
	}
	return rows
}

// floorOmission is a floor that left out 120 messages from 2026-09-01 08:00
// to 2026-10-20 17:42 UTC.
func floorOmission() sqlc.MarkMessagesOmittedRow {
	return sqlc.MarkMessagesOmittedRow{
		OmittedCount:   120,
		FirstCreatedAt: pgtype.Timestamptz{Time: time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC), Valid: true},
		LastCreatedAt:  pgtype.Timestamptz{Time: time.Date(2026, 10, 20, 17, 42, 0, 0, time.UTC), Valid: true},
	}
}

const floorNoticeText = "120 earlier messages of this conversation, from 2026-09-01 08:00 UTC to 2026-10-20 17:42 UTC, are left out of this context and were not summarized."

// committedFloorLog is a committed floor as the lineage reads it back.
func committedFloorLog(cfg TriggerConfig, arg sqlc.CompleteCompactionLogParams) sqlc.BotHistoryMessageCompact {
	return sqlc.BotHistoryMessageCompact{
		ID:            arg.ID,
		BotID:         pgtype.UUID{Bytes: uuid.MustParse(cfg.BotID), Valid: true},
		SessionID:     pgtype.UUID{Bytes: uuid.MustParse(cfg.SessionID), Valid: true},
		Status:        arg.Status,
		Summary:       arg.Summary,
		MessageCount:  arg.MessageCount,
		Coverage:      arg.Coverage,
		AnchorStartMs: arg.AnchorStartMs,
		AnchorEndMs:   arg.AnchorEndMs,
		FloorMs:       arg.FloorMs,
		FloorNotice:   arg.FloorNotice,
	}
}

// A history no summary has reached stands on a floor first: the turns before
// what the last request read are left out with a notice, and the pass after
// it folds the rest onto the floor, whose notice it inherits without the
// summarizer ever reading it.
func TestCompactStandsALegacyBacklogOnAFloorBeforeFolding(t *testing.T) {
	t.Parallel()

	stub := &stubModel{summary: "what the kept turns said"}
	cfg := machineryConfig(stub, 100000)
	rows := floorRows(t, cfg)
	current := rows[len(rows)-1]
	cfg.ProtectedSources = historySources(current)
	cfg.Anchors = []turn.ContextMessageSource{{Kind: "external", ID: "inbound-1"}}
	q := &fakeQueries{
		uncompacted: rows,
		floorTurn:   &sqlc.GetCompactionFloorTurnRow{FloorTurnPosition: 41, KeptTokens: 9000},
		omitted:     floorOmission(),
	}
	q.onComplete = func() {
		if arg := q.completeCalls[len(q.completeCalls)-1]; arg.Status == "ok" && arg.FloorMs > 0 {
			q.priorLogs = append(q.priorLogs, committedFloorLog(cfg, arg))
		}
	}

	passes, err := newMachineryService(q).Compact(context.Background(), cfg)
	if err != nil || len(passes) != 2 {
		t.Fatalf("passes=%d err=%v, want the floor and one fold", len(passes), err)
	}
	if len(q.floorArgs) != 1 {
		t.Fatalf("floor lookups = %d, want 1", len(q.floorArgs))
	}
	lookup := q.floorArgs[0]
	if lookup.HistoryBudgetTokens != 100000 || lookup.BytesPerToken != contextfrag.EstimateBytesPerToken || lookup.CreatedBefore.Valid ||
		!slices.Equal(lookup.ProtectedMessageIds, []pgtype.UUID{current.ID}) || !slices.Equal(lookup.ProtectedExternalMessageIds, []string{"inbound-1"}) {
		t.Fatalf("floor lookup = %+v, want the budget and every protected row and inbound message", lookup)
	}
	if len(q.omitArgs) != 1 || q.omitArgs[0].TurnPosition != 41 || q.omitArgs[0].CreatedBefore.Valid {
		t.Fatalf("omissions = %+v, want the turns up to the floor", q.omitArgs)
	}
	floor := q.completeCalls[0]
	lastMs := floorOmission().LastCreatedAt.Time.UnixMilli()
	if floor.Status != "ok" || floor.Summary != floorNoticeText || floor.FloorNotice != floorNoticeText || floor.MessageCount != 120 ||
		string(floor.Coverage) != "[]" || floor.FloorMs != lastMs || floor.AnchorEndMs != lastMs ||
		floor.AnchorStartMs != floorOmission().FirstCreatedAt.Time.UnixMilli() || floor.Usage != nil || floor.ModelID.Valid {
		t.Fatalf("floor = %+v, want an unsummarized notice spanning the rows it left out", floor)
	}
	if passes[0].ArtifactID != formatUUID(floor.ID) || passes[0].Summary != floorNoticeText || len(passes[0].Covered) != 0 || len(passes[0].Absorbed) != 0 {
		t.Fatalf("floor pass = %+v", passes[0])
	}

	fold := q.rollupCompleted
	want := floorNoticeText + "\n\n" + stub.summary
	if fold.Summary != want || passes[1].Summary != want {
		t.Fatalf("fold summary = %q, pass summary = %q, want %q", fold.Summary, passes[1].Summary, want)
	}
	if !slices.Equal(fold.Parents, []pgtype.UUID{floor.ID}) || fold.FloorMs != lastMs || fold.FloorNotice != floorNoticeText ||
		fold.AnchorStartMs != floor.AnchorStartMs {
		t.Fatalf("fold = %+v, want it to stand on the floor it absorbs", fold)
	}
	if strings.Contains(stub.prompt, "earlier messages of this conversation") || strings.Contains(stub.prompt, "<earlier_summary>\n") {
		t.Fatalf("the summarizer read the floor notice:\n%s", stub.prompt)
	}
}

// A history that already stands on a floor gets no second one; the fold
// keeps the floor's notice in front of what the summarizer writes.
func TestCompactStandsAFlooredHistoryOnNoSecondFloor(t *testing.T) {
	t.Parallel()

	stub := &stubModel{summary: "what followed"}
	cfg := machineryConfig(stub, 100000)
	rows := floorRows(t, cfg)
	cfg.ProtectedSources = historySources(rows[len(rows)-1])
	omission := floorOmission()
	floor := committedFloorLog(cfg, sqlc.CompleteCompactionLogParams{
		ID: testUUID(t), Status: "ok", Summary: floorNoticeText + "\n\nwhat came before", MessageCount: 120, Coverage: []byte("[]"),
		AnchorStartMs: omission.FirstCreatedAt.Time.UnixMilli(), AnchorEndMs: omission.LastCreatedAt.Time.UnixMilli(),
		FloorMs: omission.LastCreatedAt.Time.UnixMilli(), FloorNotice: floorNoticeText,
	})
	q := &fakeQueries{
		uncompacted: rows,
		priorLogs:   []sqlc.BotHistoryMessageCompact{floor},
		floorTurn:   &sqlc.GetCompactionFloorTurnRow{FloorTurnPosition: 41},
	}

	passes, err := newMachineryService(q).Compact(context.Background(), cfg)
	if err != nil || len(passes) != 1 {
		t.Fatalf("passes=%d err=%v, want one fold", len(passes), err)
	}
	if len(q.floorArgs) != 0 || len(q.omitArgs) != 0 {
		t.Fatalf("looked for another floor: lookups=%d omissions=%d", len(q.floorArgs), len(q.omitArgs))
	}
	if want := floorNoticeText + "\n\n" + stub.summary; q.rollupCompleted.Summary != want || q.rollupCompleted.FloorNotice != floorNoticeText {
		t.Fatalf("fold = %+v, want %q", q.rollupCompleted, want)
	}
	if !strings.Contains(stub.prompt, "<earlier_summary>\nwhat came before\n</earlier_summary>") || strings.Contains(stub.prompt, floorNoticeText) {
		t.Fatalf("the summarizer must read the floored summary without its notice:\n%s", stub.prompt)
	}
}

// A run's in-request fold reads only what its request renders, so it never
// sets a floor.
func TestCompactSetsNoFloorForARequestView(t *testing.T) {
	t.Parallel()

	stub := &stubModel{summary: "folded view"}
	cfg := machineryConfig(stub, 100000)
	rows := floorRows(t, cfg)
	for _, row := range rows {
		cfg.View = append(cfg.View, turn.ContextMessageSource{Kind: "history", ID: row.ID.String()})
	}
	cfg.ProtectedSources = historySources(rows[len(rows)-1])
	q := &fakeQueries{uncompacted: rows, floorTurn: &sqlc.GetCompactionFloorTurnRow{FloorTurnPosition: 41}, omitted: floorOmission()}

	if _, err := newMachineryService(q).Compact(context.Background(), cfg); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(q.floorArgs) != 0 || len(q.omitArgs) != 0 {
		t.Fatalf("a request view set a floor: lookups=%d omissions=%d", len(q.floorArgs), len(q.omitArgs))
	}
}

// A floor whose turns another writer folded first leaves nothing out and
// commits nothing; the event folds as it would without it.
func TestCompactFoldsWithoutAFloorThatLeavesNothingOut(t *testing.T) {
	t.Parallel()

	stub := &stubModel{summary: "folded without a floor"}
	cfg := machineryConfig(stub, 100000)
	rows := floorRows(t, cfg)
	cfg.ProtectedSources = historySources(rows[len(rows)-1])
	q := &fakeQueries{uncompacted: rows, floorTurn: &sqlc.GetCompactionFloorTurnRow{FloorTurnPosition: 41}}

	passes, err := newMachineryService(q).Compact(context.Background(), cfg)
	if err != nil || len(passes) != 1 || passes[0].Summary != stub.summary {
		t.Fatalf("passes=%+v err=%v, want one fold without a notice", passes, err)
	}
	if len(q.omitArgs) != 1 || q.completeCalls[0].Status != "error" || q.completeCalls[0].FloorMs != 0 {
		t.Fatalf("the empty floor's attempt = %+v, want it recorded as failed", q.completeCalls)
	}
}

// A floor's notice says how many messages it left out and when, to the
// minute, they were written.
func TestFloorNoticeSaysWhatItLeftOut(t *testing.T) {
	t.Parallel()

	first := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		count int64
		last  time.Time
		want  string
	}{
		{1, first, "1 earlier message of this conversation, from 2026-09-01 08:00 UTC, is left out of this context and was not summarized."},
		{40, first.Add(39 * time.Second), "40 earlier messages of this conversation, from 2026-09-01 08:00 UTC, are left out of this context and were not summarized."},
		{120, time.Date(2026, 10, 20, 17, 42, 0, 0, time.UTC), floorNoticeText},
	} {
		if got := floorNotice(tc.count, first, tc.last); got != tc.want {
			t.Errorf("floorNotice(%d) = %q, want %q", tc.count, got, tc.want)
		}
	}
}

func TestArtifactFromDBRowChecksTheAnchorsOfAFloor(t *testing.T) {
	t.Parallel()

	above := mustMarshalCoverage(t, strictTestCoveredSource("above", 3000))
	floorRow := func(startMs, endMs int64, coverage []byte) sqlc.BotHistoryMessageCompact {
		return sqlc.BotHistoryMessageCompact{
			ID: testUUID(t), Status: "ok", Summary: "notice", Coverage: coverage,
			AnchorStartMs: startMs, AnchorEndMs: endMs, FloorMs: 2500, FloorNotice: "notice",
		}
	}
	for _, tc := range []struct {
		name      string
		row       sqlc.BotHistoryMessageCompact
		malformed bool
	}{
		{"floor alone", floorRow(1000, 2500, []byte("[]")), false},
		{"floor that ends past its rows", floorRow(1000, 2600, []byte("[]")), true},
		{"floor that starts above itself", floorRow(2600, 2600, []byte("[]")), true},
		{"floor without a start", floorRow(0, 2500, []byte("[]")), true},
		{"floor under the coverage", floorRow(1000, 3000, above), false},
		{"floor under coverage it ends before", floorRow(1000, 2500, above), true},
		{"floor that starts above its coverage", floorRow(3500, 3000, above), true},
	} {
		artifact, err := artifactFromDBRow(tc.row)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if artifact.CoverageMalformed != tc.malformed {
			t.Errorf("%s: malformed = %t, want %t", tc.name, artifact.CoverageMalformed, tc.malformed)
		}
	}
}

// A floor joins a lineage though it lists nothing it covers; a successor
// that does not stand on its parent's floor breaks the lineage.
func TestFrontierKeepsTheFloorAcrossItsLineage(t *testing.T) {
	t.Parallel()

	owner := ArtifactOwner{BotID: uuid.NewString(), SessionID: uuid.NewString(), SessionIDKnown: true}
	bot, session := pgtype.UUID{Bytes: uuid.MustParse(owner.BotID), Valid: true}, pgtype.UUID{Bytes: uuid.MustParse(owner.SessionID), Valid: true}
	floorID, successorID := testUUID(t), testUUID(t)
	floor := sqlc.BotHistoryMessageCompact{
		ID: floorID, BotID: bot, SessionID: session, Status: "ok", Summary: "notice", Coverage: []byte("[]"),
		AnchorStartMs: 1000, AnchorEndMs: 2500, FloorMs: 2500, FloorNotice: "notice",
		SupersededBy: successorID, SupersededAt: pgtype.Timestamptz{Time: time.Unix(10, 0), Valid: true},
	}
	successor := sqlc.BotHistoryMessageCompact{
		ID: successorID, BotID: bot, SessionID: session, Status: "ok", Summary: "notice\n\nwhat followed",
		Coverage: mustMarshalCoverage(t, strictTestCoveredSource("above", 3000)), AnchorStartMs: 1000, AnchorEndMs: 3000,
		FloorMs: 2500, FloorNotice: "notice", ParentIds: []pgtype.UUID{floorID}, ArtifactLevel: 1,
	}

	frontier := buildArtifactFrontierForOwner(fusionArtifacts(t, []sqlc.BotHistoryMessageCompact{floor, successor}), owner)
	if len(frontier.Issues) != 0 || len(frontier.Artifacts) != 1 || frontier.Artifacts[0].ID != formatUUID(successorID) {
		t.Fatalf("frontier = %+v, issues = %v, want the successor standing on the floor", frontier.Artifacts, frontier.Issues)
	}
	if resolved, ok := frontier.Resolve(formatUUID(floorID)); !ok || resolved.FloorMs != 2500 {
		t.Fatalf("the floor resolves to %+v (%t), want its successor", resolved, ok)
	}

	successor.FloorMs, successor.FloorNotice, successor.AnchorStartMs = 0, "", 3000
	frontier = buildArtifactFrontierForOwner(fusionArtifacts(t, []sqlc.BotHistoryMessageCompact{floor, successor}), owner)
	if len(frontier.Artifacts) != 0 || len(frontier.Issues) == 0 || frontier.Issues[0].Kind != LineageIssueCoverageMismatch {
		t.Fatalf("frontier = %+v, issues = %v, want a successor below its parent's floor rejected", frontier.Artifacts, frontier.Issues)
	}
}
