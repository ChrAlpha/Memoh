package transcript

import (
	"slices"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/agenttest"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runreq"
)

const stage4KeepTokens = 600

// A compacted session reaches the model as the summary followed by every row
// after the boundary, in order, and nothing the summary stands for. A second
// compaction supersedes the first summary rather than stacking on it. This
// guards the stage-4 ruling that chat context is read only from the row table
// plus the compaction artifact (ruling 9), against real PostgreSQL rows and a
// real compaction run.
func TestStage4CompactedSessionAssemblesSummaryThenBoundaryRows(t *testing.T) {
	s := newStage4Session(t)
	s.PersistConversation("a", 10)
	s.Compact(stage4KeepTokens)
	covered, raw := agenttest.RequireCompactionSplit(t, s.CompactionSession)

	assembled := s.Assemble(runreq.ChatRequest{})
	want := append([]string{"SUMMARY-1"}, raw...)
	if got := agenttest.CompactionLabels(assembled.Messages); !slices.Equal(got, want) {
		t.Fatalf("context = %v, want %v (covered rows %v must not appear)", got, want, covered)
	}
	if kind := assembled.Records[0].SourceKind; kind != historyfrag.SourceCompactionLog {
		t.Fatalf("first record is %q, want the compaction summary", kind)
	}

	s.PersistConversation("b", 8)
	s.Compact(stage4KeepTokens)
	covered, raw = agenttest.RequireCompactionSplit(t, s.CompactionSession)
	assembled = s.Assemble(runreq.ChatRequest{})
	want = append([]string{"SUMMARY-1", "SUMMARY-2"}, raw...)
	if got := agenttest.CompactionLabels(assembled.Messages); !slices.Equal(got, want) {
		t.Fatalf("context with two summaries = %v, want %v (covered rows %v)", got, want, covered)
	}

	// The third compaction folds the two summaries into one that replaces them.
	s.PersistConversation("c", 8)
	s.Compact(stage4KeepTokens)
	covered, raw = agenttest.RequireCompactionSplit(t, s.CompactionSession)
	assembled = s.Assemble(runreq.ChatRequest{})
	want = append([]string{"SUMMARY-3"}, raw...)
	if got := agenttest.CompactionLabels(assembled.Messages); !slices.Equal(got, want) {
		t.Fatalf("context after the summaries were superseded = %v, want %v (covered rows %v)", got, want, covered)
	}
}

// Nothing is dropped by age (ruling 2, the 24-hour window deleted in stage 4):
// a session left alone for a day still assembles to its summary and the rows
// after the boundary. The rows are written 26 hours back, so the summary's
// coverage and anchor are that old too, and the summary's own timestamps are
// moved back 25 hours.
func TestStage4ContextHasNoTimeWindow(t *testing.T) {
	s := newStage4Session(t)
	s.Clock = time.Now().Add(-26 * time.Hour)
	s.PersistConversation("a", 10)
	s.Compact(stage4KeepTokens)
	_, raw := agenttest.RequireCompactionSplit(t, s.CompactionSession)

	if _, err := s.Pool.Exec(s.Ctx, `
		UPDATE bot_history_message_compacts
		SET started_at = started_at - interval '25 hours', completed_at = completed_at - interval '25 hours'
		WHERE session_id = $1`, s.SessionID); err != nil {
		t.Fatal(err)
	}

	want := append([]string{"SUMMARY-1"}, raw...)
	if got := agenttest.CompactionLabels(s.Assemble(runreq.ChatRequest{}).Messages); !slices.Equal(got, want) {
		t.Fatalf("context 25 hours later = %v, want %v", got, want)
	}
}

// A retry or edit re-runs a turn from its request: rows from the answer
// onward are cut by row id, the request row stays required, and the summary
// of the older history is still there (ruling 9, the retry/edit cutoff step
// of the assembly).
func TestStage4RetryCutoffKeepsSummaryAndDropsLaterRows(t *testing.T) {
	s := newStage4Session(t)
	s.PersistConversation("a", 10)
	s.Compact(stage4KeepTokens)
	_, raw := agenttest.RequireCompactionSplit(t, s.CompactionSession)

	ask := s.Persist("user", "ask")
	answer := s.Persist("assistant", "answer")
	s.Persist("user", "later-question")
	s.Persist("assistant", "later-answer")

	got := agenttest.CompactionLabels(s.Assemble(runreq.ChatRequest{
		HistoryCutoffBeforeMessageID: answer,
		RequiredHistoryMessageID:     ask,
	}).Messages)
	want := append(append([]string{"SUMMARY-1"}, raw...), "ask")
	if !slices.Equal(got, want) {
		t.Fatalf("retry context = %v, want %v", got, want)
	}
}

// A resume request that cannot name its persisted row (a record from before
// the turn was tracked) keeps the saved user row in history; one that does
// (ruling 14) has the row taken out, because the resume instruction supplies
// the task. The provider-request count is in resume_dedup_test.go.
func TestStage4ResumedRequestHasTheOriginalUserRowOnce(t *testing.T) {
	s := newStage4Session(t)
	original := s.Persist("user", "original-task")
	s.Persist("assistant", "partial-work")

	resume := runreq.ChatRequest{UserMessagePersisted: true, ShutdownResume: true, SkipMemoryExtraction: true, SkipTitleGeneration: true}
	if got, want := agenttest.CompactionLabels(s.Assemble(resume).Messages), []string{"original-task", "partial-work"}; !slices.Equal(got, want) {
		t.Fatalf("resume context = %v, want %v", got, want)
	}

	named := resume
	named.PersistedUserMessageID = original
	if got, want := agenttest.CompactionLabels(s.Assemble(named).Messages), []string{"partial-work"}; !slices.Equal(got, want) {
		t.Fatalf("context of a request naming its persisted row = %v, want %v", got, want)
	}
}
