package application

import (
	"slices"
	"testing"
	"time"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
)

const stage4KeepTokens = 600

func requireStage4Split(t *testing.T, s *stage4Session) (covered, raw []string) {
	t.Helper()
	covered, raw = s.compactedLabels()
	if len(covered) == 0 || len(raw) == 0 {
		t.Fatalf("fixture needs rows on both sides of the boundary, covered=%v raw=%v", covered, raw)
	}
	return covered, raw
}

// A compacted session reaches the model as the summary followed by every row
// after the boundary, in order, and nothing the summary stands for. A second
// compaction supersedes the first summary rather than stacking on it. This
// guards the stage-4 ruling that chat context is read only from the row table
// plus the compaction artifact (ruling 9), against real PostgreSQL rows and a
// real compaction run.
func TestStage4CompactedSessionAssemblesSummaryThenBoundaryRows(t *testing.T) {
	s := newStage4Session(t)
	s.persistConversation("a", 10)
	s.compact(stage4KeepTokens)
	covered, raw := requireStage4Split(t, s)

	assembled := s.assemble(ChatRequest{})
	want := append([]string{"SUMMARY-1"}, raw...)
	if got := stage4Labels(assembled.Messages); !slices.Equal(got, want) {
		t.Fatalf("context = %v, want %v (covered rows %v must not appear)", got, want, covered)
	}
	if kind := assembled.Records[0].SourceKind; kind != historyfrag.SourceCompactionLog {
		t.Fatalf("first record is %q, want the compaction summary", kind)
	}

	s.persistConversation("b", 8)
	s.compact(stage4KeepTokens)
	covered, raw = requireStage4Split(t, s)
	assembled = s.assemble(ChatRequest{})
	want = append([]string{"SUMMARY-1", "SUMMARY-2"}, raw...)
	if got := stage4Labels(assembled.Messages); !slices.Equal(got, want) {
		t.Fatalf("context with two summaries = %v, want %v (covered rows %v)", got, want, covered)
	}

	// The third compaction folds the two summaries into one that replaces them.
	s.persistConversation("c", 8)
	s.compact(stage4KeepTokens)
	covered, raw = requireStage4Split(t, s)
	assembled = s.assemble(ChatRequest{})
	want = append([]string{"SUMMARY-3"}, raw...)
	if got := stage4Labels(assembled.Messages); !slices.Equal(got, want) {
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
	s.clock = time.Now().Add(-26 * time.Hour)
	s.persistConversation("a", 10)
	s.compact(stage4KeepTokens)
	_, raw := requireStage4Split(t, s)

	if _, err := s.pool.Exec(s.ctx, `
		UPDATE bot_history_message_compacts
		SET started_at = started_at - interval '25 hours', completed_at = completed_at - interval '25 hours'
		WHERE session_id = $1`, s.sessionID); err != nil {
		t.Fatal(err)
	}

	want := append([]string{"SUMMARY-1"}, raw...)
	if got := stage4Labels(s.assemble(ChatRequest{}).Messages); !slices.Equal(got, want) {
		t.Fatalf("context 25 hours later = %v, want %v", got, want)
	}
}

// A retry or edit re-runs a turn from its request: rows from the answer
// onward are cut by row id, the request row stays required, and the summary
// of the older history is still there (ruling 9, the retry/edit cutoff step
// of the assembly).
func TestStage4RetryCutoffKeepsSummaryAndDropsLaterRows(t *testing.T) {
	s := newStage4Session(t)
	s.persistConversation("a", 10)
	s.compact(stage4KeepTokens)
	_, raw := requireStage4Split(t, s)

	ask := s.persist("user", "ask")
	answer := s.persist("assistant", "answer")
	s.persist("user", "later-question")
	s.persist("assistant", "later-answer")

	got := stage4Labels(s.assemble(ChatRequest{
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
// the task. The provider-request count is in session_resume_dedup_test.go.
func TestStage4ResumedRequestHasTheOriginalUserRowOnce(t *testing.T) {
	s := newStage4Session(t)
	original := s.persist("user", "original-task")
	s.persist("assistant", "partial-work")

	resume := ChatRequest{UserMessagePersisted: true, ShutdownResume: true, SkipMemoryExtraction: true, SkipTitleGeneration: true}
	if got, want := stage4Labels(s.assemble(resume).Messages), []string{"original-task", "partial-work"}; !slices.Equal(got, want) {
		t.Fatalf("resume context = %v, want %v", got, want)
	}

	named := resume
	named.PersistedUserMessageID = original
	if got, want := stage4Labels(s.assemble(named).Messages), []string{"partial-work"}; !slices.Equal(got, want) {
		t.Fatalf("context of a request naming its persisted row = %v, want %v", got, want)
	}
}
