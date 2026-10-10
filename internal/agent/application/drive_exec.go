package application

import (
	"context"
	"encoding/json"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// runExecutor runs one turn on one runtime. The native runtime and the
// External Agent drivers both run behind it; the driver owns everything
// around the turn.
type runExecutor interface {
	// execute runs the turn and hands every event the runtime produces to emit,
	// in order, except the terminal event. It returns once the producer has
	// stopped, with the terminal event and the history write that has to
	// succeed before the terminal is published.
	execute(ctx context.Context, emit func(native.StreamEvent)) execEnding
}

// execEnding is what an executor returns when it finishes.
type execEnding struct {
	// Terminal is the runtime's terminal event; nil when the producer ended
	// without one (a native setup failure only emits Error).
	Terminal *native.StreamEvent
	// Failure is a failure the executor named itself: an External Agent
	// driver's Prompt error, an anchor merge that failed. The executor names it
	// because it knows where the failure came from; a driver's own failure
	// carries its source in the cause (a runtime prompt failure does not read
	// like this process's own), and re-deriving the name here would lose that.
	Failure *RunFailure
	// Cause is a failure the executor saw outside the event stream but did not
	// name, such as a step that failed to commit. It is named from Phase.
	Cause error
	// Phase names the stage Cause belongs to: the ending's history write
	// (phaseCommit) for a round-persist failure, execution (phaseExecution)
	// for a step-commit failure. It is what keeps a failed history write named
	// session_runtime.history_inconsistent instead of runtime_run_failed, and
	// keeps it from writing a failure row.
	Phase runPhase
	// Commit writes this ending's history. The driver calls it at most once,
	// after naming the run's failure, never after ownership loss.
	Commit func(ctx context.Context, f *RunFailure, row bool) (commitSummary, error)
	// Deferred is set when the run parked awaiting a decision.
	Deferred bool
	// Cleanup removes state this ending's history write does not own. The
	// driver calls it once, after Commit and even when ownership was lost.
	Cleanup func(ctx context.Context)
}

// commitSummary is what an ending's history write produced.
type commitSummary struct {
	Persisted []messagepkg.Message
	Text      string
	Usage     json.RawMessage
	ModelID   string
	// UsageTokens is the final input token count the ending's history write
	// used, so the terminal record and the schedule result report the same
	// number the run was accounted with.
	UsageTokens int
}
