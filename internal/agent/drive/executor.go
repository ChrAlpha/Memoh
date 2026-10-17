package drive

import (
	"context"
	"encoding/json"

	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// RunExecutor runs one turn on one runtime. The native runtime and the
// External Agent drivers both run behind it; the driver owns everything
// around the turn.
type RunExecutor interface {
	// Execute runs the turn and hands every event the runtime produces to emit,
	// in order, except the terminal event. It returns once the producer has
	// stopped, with the terminal event and the history write that has to
	// succeed before the terminal is published.
	Execute(ctx context.Context, emit func(native.StreamEvent)) ExecEnding
}

// ExecEnding is what an executor returns when it finishes.
type ExecEnding struct {
	// Terminal is the runtime's terminal event; nil when the producer ended
	// without one (a native setup failure only emits Error).
	Terminal *native.StreamEvent
	// Failure is a failure the executor named itself: an External Agent
	// driver's Prompt error, an anchor merge that failed. The executor names it
	// because it knows where the failure came from; a driver's own failure
	// carries its source in the cause (a runtime prompt failure does not read
	// like this process's own), and re-deriving the name here would lose that.
	Failure *outcome.RunFailure
	// Cause is a failure the executor saw outside the event stream but did not
	// name, such as a step that failed to commit. It is named from Phase.
	Cause error
	// Phase names the stage Cause belongs to: the ending's history write
	// (outcome.PhaseCommit) for a round-persist failure, execution (outcome.PhaseExecution)
	// for a step-commit failure. It is what keeps a failed history write named
	// session_runtime.history_inconsistent instead of runtime_run_failed, and
	// keeps it from writing a failure row.
	Phase outcome.RunPhase
	// Commit writes this ending's history. The driver calls it at most once,
	// after naming the run's failure, never after a stop or ownership loss.
	Commit func(ctx context.Context, f *outcome.RunFailure, row bool) (CommitSummary, error)
	// CommitStopped writes what the user already saw of a run that was stopped:
	// no failure, no failure row, no memory extraction. The driver calls it at
	// most once, in place of Commit, when the run ended as a stop. An executor
	// that writes its history while it runs (the native agent checkpoints the
	// step it was in when the stop arrived) has nothing left to write and leaves
	// it nil; one that learns what the turn produced only when it returns writes
	// that here.
	CommitStopped func(ctx context.Context) (CommitSummary, error)
	// Deferred is set when the run parked awaiting a decision.
	Deferred bool
	// Cleanup removes state this ending's history write does not own. The
	// driver calls it once, after Commit and even when ownership was lost.
	Cleanup func(ctx context.Context)
}

// CommitSummary is what an ending's history write produced.
type CommitSummary struct {
	Persisted []messagepkg.Message
	Text      string
	Usage     json.RawMessage
	ModelID   string
	// UsageTokens is the final input token count the ending's history write
	// used, so the terminal record and the schedule result report the same
	// number the run was accounted with.
	UsageTokens int
}
