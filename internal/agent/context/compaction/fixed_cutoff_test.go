package compaction

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// markedRow is a history row whose text carries marker, created at createdMs.
func markedRow(t *testing.T, role, marker string, createdMs int64) sqlc.ListUncompactedMessagesBySessionRow {
	t.Helper()
	row := mkRow(t, role, `"`+marker+strings.Repeat("x", 396)+`"`, 0)
	row.CreatedAt = pgtype.Timestamptz{Time: time.UnixMilli(createdMs), Valid: true}
	return row
}

// fixedTargetRows is eleven 100-token rows and a 40-token user message that
// preserve-recent keeps, so eleven rows are outside the protected tail.
func fixedTargetRows(t *testing.T) []sqlc.ListUncompactedMessagesBySessionRow {
	t.Helper()
	rows := make([]sqlc.ListUncompactedMessagesBySessionRow, 0, 12)
	for i := 0; i < 11; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		rows = append(rows, textRow(t, role, 100))
	}
	return append(rows, mkRow(t, "user", `"current"`, 40))
}

// The fixed part of the request is subtracted from the target before the rows
// are selected. A fixed part that already exceeds the target claims every row
// the preserve-recent tail does not keep.
func TestSyncTargetLeavesTheFixedRequestOutOfTheRowTarget(t *testing.T) {
	t.Parallel()

	marked := func(t *testing.T, fixed int) int {
		t.Helper()
		rows := fixedTargetRows(t)
		stub := &stubModel{summary: "s"}
		cfg := machineryConfig(stub, 250)
		cfg.FixedTokens = fixed
		q := &fakeQueries{uncompacted: rows}
		res, err := newMachineryService(q).RunCompactionSync(context.Background(), cfg)
		if err != nil {
			t.Fatalf("RunCompactionSync: %v", err)
		}
		if len(q.markedIDs) == 0 {
			if res.Status != StatusNoop || res.Reason == "" {
				t.Fatalf("no rows claimed: result = %+v, want a noop with a reason", res)
			}
		}
		return len(q.markedIDs)
	}

	without := marked(t, 0)
	with := marked(t, 1000)
	if with <= without {
		t.Fatalf("fixed request above the target claimed %d rows, want more than the %d claimed without it", with, without)
	}
	if with != len(fixedTargetRows(t))-1 {
		t.Fatalf("fixed request above the target claimed %d rows, want every row outside the preserve-recent tail (%d)",
			with, len(fixedTargetRows(t))-1)
	}
}

func TestSyncNoopNamesNoCandidatesWhenEveryRowIsProtected(t *testing.T) {
	t.Parallel()

	stub := &stubModel{summary: "s"}
	q := &fakeQueries{uncompacted: []sqlc.ListUncompactedMessagesBySessionRow{
		mkRow(t, "user", `"only the current message"`, 40),
	}}
	res, err := newMachineryService(q).RunCompactionSync(context.Background(), machineryConfig(stub, 100))
	if err != nil {
		t.Fatalf("RunCompactionSync: %v", err)
	}
	if res.Status != StatusNoop || res.Reason != ReasonNoCandidates {
		t.Fatalf("result = %+v, want noop/%s", res, ReasonNoCandidates)
	}
	if stub.calls != 0 || len(q.markedIDs) != 0 {
		t.Fatalf("a noop must not summarize or claim (calls=%d marked=%d)", stub.calls, len(q.markedIDs))
	}
}

func TestSyncNoopNamesBelowTargetWhenUnprotectedRowsFitTheTarget(t *testing.T) {
	t.Parallel()

	stub := &stubModel{summary: "s"}
	q := &fakeQueries{uncompacted: fixedTargetRows(t)}
	res, err := newMachineryService(q).RunCompactionSync(context.Background(), machineryConfig(stub, 1_000_000))
	if err != nil {
		t.Fatalf("RunCompactionSync: %v", err)
	}
	if res.Status != StatusNoop || res.Reason != ReasonBelowTarget {
		t.Fatalf("result = %+v, want noop/%s", res, ReasonBelowTarget)
	}
}

// A replacement's cutoff bounds the summary to history before the replaced
// turn: rows from the cutoff on are not claimed, and a prior summary that
// covers them is not carried into the prompt.
func TestSyncCutoffKeepsRowsAndPriorSummariesAfterTheReplacedTurnOut(t *testing.T) {
	t.Parallel()

	stub := &stubModel{summary: "s"}
	cfg := machineryConfig(stub, 150)
	cfg.HistoryCutoff = &HistoryCutoff{BeforeMs: 4000}
	rows := []sqlc.ListUncompactedMessagesBySessionRow{
		markedRow(t, "user", "PRE-ROW-A ", 1000),
		markedRow(t, "assistant", "PRE-ROW-B ", 2000),
		markedRow(t, "user", "PRE-ROW-C ", 3000),
		markedRow(t, "assistant", "AFTER-ROW-D ", 5000),
		markedRow(t, "user", "AFTER-ROW-E ", 6000),
	}
	early := priorLogFor(t, cfg, "EARLY-PRIOR-SUMMARY")
	early.AnchorStartMs, early.AnchorEndMs = 100, 900
	late := priorLogFor(t, cfg, "LATE-PRIOR-SUMMARY")
	late.AnchorStartMs, late.AnchorEndMs = 4500, 5500
	q := &fakeQueries{uncompacted: rows, priorLogs: []sqlc.BotHistoryMessageCompact{early, late}}

	if _, err := newMachineryService(q).RunCompactionSync(context.Background(), cfg); err != nil {
		t.Fatalf("RunCompactionSync: %v", err)
	}
	if len(q.markedIDs) == 0 {
		t.Fatal("no history before the cutoff was claimed")
	}
	before := map[pgtype.UUID]bool{rows[0].ID: true, rows[1].ID: true, rows[2].ID: true}
	for _, id := range q.markedIDs {
		if !before[id] {
			t.Fatalf("claimed a row at or after the cutoff: %v", id)
		}
	}
	if strings.Contains(stub.prompt, "AFTER-ROW") {
		t.Fatalf("a row at or after the cutoff reached the summarizer:\n%s", stub.prompt)
	}
	if strings.Contains(stub.prompt, "LATE-PRIOR-SUMMARY") {
		t.Fatalf("a prior summary covering the replaced turn reached the summarizer:\n%s", stub.prompt)
	}
	if !strings.Contains(stub.prompt, "EARLY-PRIOR-SUMMARY") {
		t.Fatalf("a prior summary before the cutoff was dropped:\n%s", stub.prompt)
	}
}
