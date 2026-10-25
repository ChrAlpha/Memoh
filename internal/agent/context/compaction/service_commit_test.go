package compaction

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/chat/checkpoint"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// A pass commits a checkpoint whose summary stands in for the rows it folds:
// it follows the checkpoint the pass read, sits at the last folded row, and
// becomes the session's active checkpoint in the transaction that settles the
// pass's log.
func TestDoCompactionCommitsACheckpointOverWhatItFolds(t *testing.T) {
	t.Parallel()

	rows := qualityRows(t)
	q := &fakeQueries{history: rows}
	stub := &stubModel{summary: "SUMMARY"}
	pass, folded, err := newMachineryService(q).runPass(context.Background(), currentConfig(stub, 200, rows[2]))
	if err != nil || !folded {
		t.Fatalf("folded=%v err=%v, want a committed pass", folded, err)
	}
	created := q.checkpoints[q.active]
	if created.ParentID.Valid || created.TurnPosition != rows[1].TurnPosition.Int64 || created.TurnMessageSeq != rows[1].TurnMessageSeq.Int64 {
		t.Fatalf("checkpoint after %v at (%d, %d), want a root through the last folded row", created.ParentID, created.TurnPosition, created.TurnMessageSeq)
	}
	if held := q.held(); !slices.Equal(held, []checkpoint.Item{{SummaryID: formatUUID(q.createdLogID)}}) {
		t.Fatalf("checkpoint holds %+v, want only the pass's summary", held)
	}
	if !slices.Equal(q.folded, []pgtype.UUID{rows[0].ID, rows[1].ID}) {
		t.Fatalf("folded %v, want the two rows before the current input", q.folded)
	}
	if pass.ArtifactID != formatUUID(q.createdLogID) || len(pass.Absorbed) != 0 || q.completed.Status != "ok" || q.completed.MessageCount != 2 {
		t.Fatalf("pass %+v log %+v, want the ok log of two rows named and nothing absorbed", pass, q.completed)
	}
	if !slices.Equal(q.queryCalls, []string{"assets", "lock"}) {
		t.Fatalf("calls = %v, want the assets read before the commit locks the session", q.queryCalls)
	}
}

// A pass folds around an anchor the run keeps in place; the checkpoint keeps
// it by reference before the summary, and the next pass folds it like any
// row once nothing anchors it.
func TestDoCompactionKeepsAnAnchorItFoldsAroundByReference(t *testing.T) {
	t.Parallel()

	rows := []candidateRow{
		mkRow(t, "user", jsonStr(strings.Repeat("old question ", 40)), 100),
		mkRow(t, "assistant", jsonStr(strings.Repeat("old answer ", 40)), 100),
		mkRow(t, "user", `"STEER"`, 100),
		mkRow(t, "assistant", jsonStr(strings.Repeat("work after the steer ", 40)), 100),
		mkRow(t, "user", `"current question"`, 100),
	}
	q := &fakeQueries{history: rows}
	cfg := currentConfig(&stubModel{summary: "SUMMARY-1"}, 400, rows[4])
	cfg.Anchors = historySources(rows[2])
	svc := newMachineryService(q)
	first, err := svc.compactOnce(context.Background(), cfg)
	if err != nil || first.Status != passOK {
		t.Fatalf("first pass = %+v err=%v", first, err)
	}
	firstSummary := formatUUID(q.createdLogID)
	if held := q.held(); !slices.Equal(held, []checkpoint.Item{{RowID: formatUUID(rows[2].ID)}, {SummaryID: firstSummary}}) {
		t.Fatalf("checkpoint holds %+v, want the anchor kept by reference, then the summary", held)
	}
	if !slices.Equal(q.folded, []pgtype.UUID{rows[0].ID, rows[1].ID, rows[3].ID}) {
		t.Fatalf("folded %v, want every row around the anchor", q.folded)
	}

	q.history = append(q.history, mkRow(t, "assistant", `"answer"`, 100), mkRow(t, "user", `"next question"`, 100))
	stub := &stubModel{summary: "SUMMARY-2"}
	cfg = currentConfig(stub, 400, q.history[len(q.history)-1])
	pass, folded, err := svc.runPass(context.Background(), cfg)
	if err != nil || !folded {
		t.Fatalf("second pass folded=%v err=%v", folded, err)
	}
	if !strings.Contains(stub.prompt, "<earlier_summary>\nSUMMARY-1\n</earlier_summary>") || !strings.Contains(stub.prompt, "STEER") {
		t.Fatalf("the second pass must read the earlier summary and the row its checkpoint keeps:\n%s", stub.prompt)
	}
	if !slices.Equal(pass.Absorbed, []string{firstSummary}) || !slices.Equal(q.held(), []checkpoint.Item{{SummaryID: pass.ArtifactID}}) {
		t.Fatalf("absorbed %v, checkpoint holds %+v, want the earlier checkpoint folded whole", pass.Absorbed, q.held())
	}
	if !slices.Equal(q.folded, []pgtype.UUID{rows[2].ID, rows[4].ID, q.history[5].ID}) {
		t.Fatalf("folded %v, want the kept anchor and the rows after the earlier checkpoint", q.folded)
	}
}

// A pass commits nothing when the history it read moved before its commit:
// another checkpoint became the session's, or a row it folds is no longer a
// context row. Its log settles as an error and the session keeps what it
// has.
func TestDoCompactionCommitsNothingOnceTheHistoryMoved(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		move func(q *fakeQueries) pgtype.UUID
	}{
		{"another checkpoint committed", func(q *fakeQueries) pgtype.UUID { return q.hold("written meanwhile").ID }},
		{"a folded row is gone", func(q *fakeQueries) pgtype.UUID {
			q.history = q.history[1:]
			return pgtype.UUID{}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := &fakeQueries{history: qualityRows(t)}
			var want pgtype.UUID
			stub := &stubModel{summary: "SUMMARY", during: func() { want = tc.move(q) }}
			_, err := newMachineryService(q).compactOnce(context.Background(), currentConfig(stub, 200, q.history[2]))
			if ReasonOf(err) != ReasonStaleSource || !errors.Is(err, checkpoint.ErrMoved) {
				t.Fatalf("err = %v, want %s from a moved checkpoint", err, ReasonStaleSource)
			}
			if q.active != want || q.logs[q.createdLogID].Status != "error" {
				t.Fatalf("checkpoint = %v log = %q, want %v kept and the log settled as an error", q.active, q.logs[q.createdLogID].Status, want)
			}
		})
	}
}

// A commit whose log cannot settle rolls back with it: the session keeps its
// checkpoint and the log settles as an error outside the transaction.
func TestDoCompactionSettlesItsLogAsAnErrorWhenItsCommitFails(t *testing.T) {
	t.Parallel()

	q := &fakeQueries{history: machineryCorpus(t), completeErrors: []error{pgx.ErrNoRows, nil}}
	parent := q.hold("earlier summary")
	stub := &stubModel{summary: "stale summary"}

	_, err := newMachineryService(q).compactOnce(context.Background(), machineryConfig(stub, 450))
	if !errors.Is(err, pgx.ErrNoRows) || ReasonOf(err) != ReasonStaleSource {
		t.Fatalf("compact error = %v, want pgx.ErrNoRows as %s", err, ReasonStaleSource)
	}
	if len(q.completeCalls) != 2 || q.completeCalls[0].Status != "ok" || q.completeCalls[1].Status != "error" {
		t.Fatalf("completions = %+v, want the commit's ok, then the terminal error", q.completeCalls)
	}
	if q.completeCalls[1].Summary != "" || q.completeCalls[1].ErrorMessage == "" {
		t.Fatalf("terminal completion leaked the summary or omitted the error: %#v", q.completeCalls[1])
	}
	if q.active != parent.ID || len(q.checkpoints) != 1 || q.logs[q.createdLogID].Status != "error" {
		t.Fatalf("checkpoint = %v of %d, log = %q: the commit must roll back whole", q.active, len(q.checkpoints), q.logs[q.createdLogID].Status)
	}
}

// A pass whose log cannot be opened, as when the session no longer belongs
// to the bot, stops before the summarizer.
func TestDoCompactionFailsBeforeTheSummarizerWhenItsLogCannotOpen(t *testing.T) {
	t.Parallel()

	q := &fakeQueries{history: machineryCorpus(t), createErr: pgx.ErrNoRows}
	stub := &stubModel{summary: "must not run"}

	_, err := newMachineryService(q).compactOnce(context.Background(), machineryConfig(stub, 450))
	if !errors.Is(err, pgx.ErrNoRows) || ReasonOf(err) != ReasonStaleSource {
		t.Fatalf("compact error = %v, want pgx.ErrNoRows as %s", err, ReasonStaleSource)
	}
	if stub.calls != 0 || q.active.Valid {
		t.Fatalf("calls=%d checkpoint=%v, want neither", stub.calls, q.active)
	}
}

func TestDoCompactionStopsWhenAssetsCannotBeLoaded(t *testing.T) {
	t.Parallel()

	rows := qualityRows(t)
	assetErr := errors.New("asset read failed")
	q := &fakeQueries{history: rows, assetsErr: assetErr}
	stub := &stubModel{summary: "SUMMARY"}

	_, err := newMachineryService(q).compactOnce(context.Background(), currentConfig(stub, 200, rows[2]))
	if !errors.Is(err, assetErr) {
		t.Fatalf("compact error = %v, want %v", err, assetErr)
	}
	if stub.calls != 0 || q.active.Valid || q.completed.Status != "error" {
		t.Fatalf("calls=%d checkpoint=%v status=%q, want an error log and nothing summarized", stub.calls, q.active, q.completed.Status)
	}
}

// An edit or retry folds only the history before its cutoff: the cutoff row
// and what follows it are not part of its request.
func TestDoCompactionReadsOnlyTheHistoryBeforeItsCutoff(t *testing.T) {
	t.Parallel()

	rows := []candidateRow{
		mkRow(t, "user", jsonStr(strings.Repeat("old question ", 40)), 100),
		mkRow(t, "assistant", jsonStr(strings.Repeat("old answer ", 40)), 100),
		mkRow(t, "user", `"REPLACED_REQUEST"`, 100),
		mkRow(t, "assistant", `"REPLACED_ANSWER"`, 100),
	}
	q := &fakeQueries{history: rows}
	q.ordered()
	stub := &stubModel{summary: "SUMMARY"}
	cfg := machineryConfig(stub, 200)
	cutoff := rowPosition(rows[2])
	cfg.CandidatesBefore = &cutoff

	if _, err := newMachineryService(q).compactOnce(context.Background(), cfg); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !slices.Equal(q.folded, []pgtype.UUID{rows[0].ID, rows[1].ID}) || strings.Contains(stub.prompt, "REPLACED_") {
		t.Fatalf("folded %v, want only the rows before the cutoff read:\n%s", q.folded, stub.prompt)
	}
}

// An edit or retry's event first rewinds the session to the checkpoint its
// replacement keeps, as the replacement's first step will, so it folds onto
// that checkpoint instead of one that depends on the replaced turn.
func TestCompactRewindsTheReplacedTurnBeforeFolding(t *testing.T) {
	t.Parallel()

	stub := &stubModel{summary: "folded before the cutoff"}
	cfg := machineryConfig(stub, 100000)
	cfg.ReplacedTurnID = uuid.NewString()
	q := &fakeQueries{history: qualityRows(t)}
	kept := q.hold("earlier")
	q.hold("depends on the replaced turn")
	q.onRewind = func() { q.active = kept.ID }

	passes, err := newMachineryService(q).Compact(context.Background(), cfg)
	if err != nil || len(passes) != 1 {
		t.Fatalf("passes=%d err=%v, want one pass after the rewind", len(passes), err)
	}
	turnID := pgtype.UUID{Bytes: uuid.MustParse(cfg.ReplacedTurnID), Valid: true}
	if len(q.rewinds) != 1 || q.rewinds[0].TurnID != turnID || q.rewinds[0].SessionID.String() != cfg.SessionID {
		t.Fatalf("rewinds=%+v, want one before the replaced turn", q.rewinds)
	}
	if !slices.Equal(passes[0].Absorbed, summaryIDs(kept)) || q.checkpoints[q.active].ParentID != kept.ID {
		t.Fatalf("absorbed=%v, want only the summary of the checkpoint the replacement keeps", passes[0].Absorbed)
	}
	if strings.Contains(stub.prompt, "depends on the replaced turn") {
		t.Fatalf("the summarizer read a summary the request does not render:\n%s", stub.prompt)
	}
}

// A request rendered on another checkpoint than the session's active one
// cannot be folded: no fold onto the active checkpoint matches what it shows.
func TestCompactRefusesAViewOfAnotherCheckpoint(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// older renders the summary of the checkpoint the active one follows.
		older bool
	}{
		{"no summary", false},
		{"an older checkpoint's summary", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows := machineryCorpus(t)
			stub := &stubModel{summary: "new-summary"}
			cfg := machineryConfig(stub, 450)
			for _, row := range rows {
				cfg.View = append(cfg.View, turn.ContextMessageSource{Kind: "history", ID: formatUUID(row.ID)})
			}
			q := &fakeQueries{history: rows}
			older := q.hold("older summary")
			q.hold("UNRENDERED_SUMMARY_MUST_NOT_ENTER_REQUEST")
			if tc.older {
				cfg.View = append(cfg.View, turn.ContextMessageSource{Kind: "summary", ID: summaryIDs(older)[0]})
			}

			passes, err := newMachineryService(q).Compact(context.Background(), cfg)
			if ReasonOf(err) != ReasonStaleSource || len(passes) != 0 || stub.calls != 0 || q.created {
				t.Fatalf("passes=%d calls=%d logged=%v err=%v, want %s before the pass starts", len(passes), stub.calls, q.created, err, ReasonStaleSource)
			}
		})
	}
}

// An event whose request renders more than one summarizer call can read
// folds in passes, each onto the checkpoint the one before committed: the
// view it keeps names the summary that pass wrote in place of the ones it
// absorbed.
func TestCompactFoldsEachPassOntoTheOneBefore(t *testing.T) {
	t.Parallel()

	rows := make([]candidateRow, 0, 13)
	for i := 0; i < 12; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		rows = append(rows, textRow(t, role, 100))
	}
	rows = append(rows, mkRow(t, "user", `"current"`, 40))
	stub := &stubModel{summary: "condensed"}
	cfg := machineryConfig(stub, 100000)
	cfg.SummaryWindowTokens = summarizerWindowFor(1000)
	cfg.ProtectedSources = historySources(rows[12])
	for _, row := range rows {
		cfg.View = append(cfg.View, turn.ContextMessageSource{Kind: "history", ID: formatUUID(row.ID)})
	}
	q := &fakeQueries{history: rows}

	passes, err := newMachineryService(q).Compact(context.Background(), cfg)
	if err != nil || len(passes) < 2 {
		t.Fatalf("passes=%d err=%v, want the history folded in passes", len(passes), err)
	}
	for i, pass := range passes[1:] {
		if !slices.Equal(pass.Absorbed, []string{passes[i].ArtifactID}) {
			t.Fatalf("pass %d absorbed %v, want the summary of pass %d", i+1, pass.Absorbed, i)
		}
	}
	last := q.checkpoints[q.active]
	if !slices.Equal(q.held(), []checkpoint.Item{{SummaryID: passes[len(passes)-1].ArtifactID}}) || rowPosition(rows[11]) != (messagepkg.HistoryPosition{Turn: last.TurnPosition, Seq: last.TurnMessageSeq}) {
		t.Fatalf("checkpoint holds %+v at (%d, %d), want the last summary through every row before the current input", q.held(), last.TurnPosition, last.TurnMessageSeq)
	}
}

// Each further pass of an event folds onto the summary the pass before
// wrote. A session moved back between them, as a row written below its
// checkpoint moves it, fails the event instead of having it fold the same
// history again.
func TestCompactFailsWhenTheSessionMovesBackBetweenPasses(t *testing.T) {
	t.Parallel()

	rows := make([]candidateRow, 0, 13)
	for i := 0; i < 12; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		rows = append(rows, textRow(t, role, 100))
	}
	rows = append(rows, mkRow(t, "user", `"current"`, 40))
	stub := &stubModel{summary: "condensed"}
	cfg := machineryConfig(stub, 100000)
	cfg.SummaryWindowTokens = summarizerWindowFor(1000)
	cfg.ProtectedSources = historySources(rows[12])
	q := rewindingQueries{&fakeQueries{history: rows}}

	passes, err := newMachineryService(q).Compact(context.Background(), cfg)
	if ReasonOf(err) != ReasonStaleSource || len(passes) != 1 {
		t.Fatalf("passes=%d err=%v, want the event to fail after its first pass", len(passes), err)
	}
}

// rewindingQueries moves the session back to no checkpoint whenever its
// active one is read.
type rewindingQueries struct{ *fakeQueries }

func (q rewindingQueries) GetActiveHistoryCheckpoint(ctx context.Context, id pgtype.UUID) (sqlc.BotHistoryCheckpoint, error) {
	q.active = pgtype.UUID{}
	return q.fakeQueries.GetActiveHistoryCheckpoint(ctx, id)
}
