package compaction

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/chat/checkpoint"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/models"
)

// A summary stands in for every summary of the checkpoint it folds, so it
// need only be shorter than all of them and the rows it folds together.
func TestDoCompactionCountsEveryEarlierSummaryAsReplaced(t *testing.T) {
	t.Parallel()

	rows := qualityRows(t)
	earlier := []string{strings.Repeat("a", 2400), strings.Repeat("b", 2400)}
	partial := foldedRowTokens(rows) + summaryReplayTokens(earlier[0])
	whole := partial + summaryReplayTokens(earlier[1])
	stub := &stubModel{summary: strings.Repeat("s", partial*4)}
	if tokens := summaryReplayTokens(stub.summary); tokens < partial || tokens >= whole {
		t.Fatalf("test setup invalid: summary=%d partial=%d whole=%d", tokens, partial, whole)
	}
	q := &fakeQueries{history: rows}
	parent := q.hold(earlier...)

	pass, folded, err := newMachineryService(q).runPass(context.Background(), machineryConfig(stub, 100000))
	if err != nil || !folded {
		t.Fatalf("folded=%v err=%v, want the summary measured against everything it replaces", folded, err)
	}
	assertFoldedWhole(t, q, pass, parent)
}

// The summarizer reads every earlier summary whole, however long, and may
// answer with as much as the model declares.
func TestDoCompactionReadsEveryEarlierSummaryWhole(t *testing.T) {
	t.Parallel()

	stub := &stubModel{summary: "one concise summary"}
	cfg := machineryConfig(stub, 100000)
	cfg.SummaryWindowTokens = 50000
	earlier := []string{
		"absorbed-oldest:" + strings.Repeat("a", 18800),
		"absorbed-middle:" + strings.Repeat("b", 4000),
		"absorbed-newest:" + strings.Repeat("c", 1200),
	}
	q := &fakeQueries{history: qualityRows(t)}
	parent := q.hold(earlier...)

	pass, folded, err := newMachineryService(q).runPass(context.Background(), cfg)
	if err != nil || !folded {
		t.Fatalf("folded=%v err=%v, want every earlier summary folded", folded, err)
	}
	if want := models.ResolveGenerationLimits(models.ClientTypeOpenAICompletions, nil, cfg.SummaryWindowTokens, 0).MaxOutputTokens; stub.maxTokens != want {
		t.Fatalf("summarizer max output tokens = %d, want the model's allowance %d", stub.maxTokens, want)
	}
	for _, summary := range earlier {
		if !strings.Contains(stub.prompt, "<earlier_summary>\n"+summary+"\n</earlier_summary>") {
			t.Fatalf("summarizer did not read the summary %q whole", summary[:16])
		}
	}
	assertFoldedWhole(t, q, pass, parent)
}

// A pass folds the summaries of the active checkpoint into one beside the
// rows after it, and refolds them alone only when their replay no longer fits
// the room the request leaves.
func TestDoCompactionFoldsTheCheckpointSummariesIntoOne(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		summaries []string
		budget    int
		newRows   bool
		wantFold  bool
	}{
		{name: "one summary and new rows", summaries: []string{"earlier decision: ship on friday"}, budget: 100000, newRows: true, wantFold: true},
		{name: "summaries that fit without new rows", summaries: []string{"earlier decision: ship on friday", "later decision: freeze on thursday"}, budget: 100000},
		{name: "summaries that no longer fit without new rows", summaries: []string{strings.Repeat("a", 2000), strings.Repeat("b", 2000)}, budget: 800, wantFold: true},
		{name: "summaries that replay within the room but past the summary limit", summaries: []string{strings.Repeat("a", 2000), strings.Repeat("b", 2000)}, budget: 1400},
		{name: "one summary without new rows", summaries: []string{"earlier decision: ship on friday"}, budget: 100000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stub := &stubModel{summary: "one replacement summary"}
			q := &fakeQueries{}
			if tc.newRows {
				q.history = qualityRows(t)
			}
			parent := q.hold(tc.summaries...)

			pass, folded, err := newMachineryService(q).runPass(context.Background(), machineryConfig(stub, tc.budget))
			if err != nil {
				t.Fatalf("compact: %v", err)
			}
			if !tc.wantFold {
				if folded || stub.calls != 0 || q.created {
					t.Fatalf("folded=%v calls=%d logged=%v, want nothing to fold and no summarizer call", folded, stub.calls, q.created)
				}
				return
			}
			if !folded || len(q.completeCalls) != 1 || q.completeCalls[0].Status != "ok" {
				t.Fatalf("folded=%v completions=%+v, want one summary in place of the checkpoint's", folded, q.completeCalls)
			}
			for _, summary := range tc.summaries {
				if !strings.Contains(stub.prompt, summary) {
					t.Fatalf("summarizer prompt omitted the folded summary %q:\n%s", summary, stub.prompt)
				}
			}
			assertFoldedWhole(t, q, pass, parent)
		})
	}
}

// A summary no shorter than the rows and every earlier summary it replaces is
// refused, and the session keeps the checkpoint it had.
func TestDoCompactionRejectsASummaryNotSmallerThanEverythingItReplaces(t *testing.T) {
	t.Parallel()

	rows := qualityRows(t)
	earlier := []string{strings.Repeat("a", 2400), strings.Repeat("b", 2400)}
	replaced := foldedRowTokens(rows) + summaryReplayTokens(earlier[0]) + summaryReplayTokens(earlier[1])
	stub := &stubModel{summary: strings.Repeat("s", replaced*4)}
	if tokens := summaryReplayTokens(stub.summary); tokens < replaced {
		t.Fatalf("test setup invalid: summary=%d replaced=%d", tokens, replaced)
	}
	q := &fakeQueries{history: rows}
	parent := q.hold(earlier...)

	_, err := newMachineryService(q).compactOnce(context.Background(), machineryConfig(stub, 100000))
	if !errors.Is(err, errIneffectiveSummary) || stub.calls != 1 {
		t.Fatalf("err=%v calls=%d, want errIneffectiveSummary after one summarizer call", err, stub.calls)
	}
	if len(q.completeCalls) != 1 || q.completeCalls[0].Status != "error" || q.active != parent.ID {
		t.Fatalf("completions=%+v checkpoint=%v, want one error log and %v kept", q.completeCalls, q.active, parent.ID)
	}
}

// A tool exchange the summarizer cannot read beside the earlier summaries
// fails the pass before it opens a log or calls the summarizer.
func TestDoCompactionFailsAnOverflowBeforeTheSummarizer(t *testing.T) {
	t.Parallel()

	const fanout = 40
	calls := make([]string, 0, fanout)
	for i := range fanout {
		calls = append(calls, fmt.Sprintf(`{"type":"tool-call","toolCallId":"c%d","toolName":"probe","input":{}}`, i))
	}
	rows := []candidateRow{mkRow(t, "assistant", "["+strings.Join(calls, ",")+"]", 100)}
	for i := range fanout {
		rows = append(rows, mkRow(t, "tool", fmt.Sprintf(`[{"type":"tool-result","toolCallId":"c%d","toolName":"probe","output":{"type":"text","value":"%s"}}]`, i, strings.Repeat("ok ", 200)), 100))
	}
	rows = append(rows, mkRow(t, "user", `"current question"`, 100))
	stub := &stubModel{summary: "must not run"}
	cfg := machineryConfig(stub, 100000)
	cfg.SummaryWindowTokens = 8000
	q := &fakeQueries{history: rows}
	parent := q.hold(strings.Repeat("a", 40), strings.Repeat("b", 40))

	_, err := newMachineryService(q).compactOnce(context.Background(), cfg)
	if !errors.Is(err, errCompactionInputOverflow) {
		t.Fatalf("compact error = %v, want errCompactionInputOverflow", err)
	}
	if stub.calls != 0 || q.created || q.active != parent.ID {
		t.Fatalf("calls=%d logged=%v checkpoint=%v, want the pass to fail before it starts", stub.calls, q.created, q.active)
	}
}

// Earlier summaries the summarizer cannot read beside the rows after them
// fail the pass before it opens a log or calls the summarizer.
func TestDoCompactionFailsEarlierSummariesItCannotRead(t *testing.T) {
	t.Parallel()

	stub := &stubModel{summary: "must not run"}
	cfg := machineryConfig(stub, 100000)
	cfg.SummaryWindowTokens = 8000
	q := &fakeQueries{history: qualityRows(t)}
	parent := q.hold(
		strings.Repeat("a", 5000),
		strings.Repeat("b", 5000),
		strings.Repeat("c", 5000),
		strings.Repeat("d", 5000),
		strings.Repeat("e", 5000),
	)

	_, err := newMachineryService(q).compactOnce(context.Background(), cfg)
	if !errors.Is(err, errCompactionInputOverflow) {
		t.Fatalf("compact error = %v, want errCompactionInputOverflow", err)
	}
	if stub.calls != 0 || q.created || q.active != parent.ID {
		t.Fatalf("calls=%d logged=%v checkpoint=%v, want the pass to fail before it starts", stub.calls, q.created, q.active)
	}
}

// foldedRowTokens is what a pass that folds every row replaces besides the
// earlier summaries.
func foldedRowTokens(rows []candidateRow) int {
	items, _ := candidatesFromRows(rows)
	entries, ids := buildEntriesAndIDs(foldableItems(items))
	entries, _ = keptAfterSummary(entries)
	return max(entriesPromptCost(coveredEntries(entries)), compactedReplayTokens(items, ids))
}

// assertFoldedWhole checks that a pass folded every summary of parent into
// the one summary the session's active checkpoint holds after it.
func assertFoldedWhole(t *testing.T, q *fakeQueries, pass Pass, parent sqlc.BotHistoryCheckpoint) {
	t.Helper()
	if !slices.Equal(pass.Absorbed, summaryIDs(parent)) {
		t.Fatalf("absorbed %v, want every summary of the checkpoint %v", pass.Absorbed, summaryIDs(parent))
	}
	if held := q.held(); !slices.Equal(held, []checkpoint.Item{{SummaryID: pass.ArtifactID}}) || q.checkpoints[q.active].ParentID != parent.ID {
		t.Fatalf("checkpoint holds %+v after %v, want only the pass's summary after %v", held, q.checkpoints[q.active].ParentID, parent.ID)
	}
}
