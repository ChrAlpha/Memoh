package application

import (
	"context"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// runSpec is what one entry decides about a run. Everything else about running
// it is the same for every entry.
type runSpec struct {
	// Run is the admitted run. A zero Handle (FencingToken <= 0) is a run nobody
	// admitted: the driver neither publishes it nor writes its terminal state.
	Run admittedRun
	// Req is the request as the entry built it: identity, route, query, turn.
	// The driver finishes the wiring the entry cannot do itself — the asset
	// collector it builds per run — and hands the result to Setup.
	Req ChatRequest
	// Setup resolves the executor from the run's request as the driver finished
	// wiring it. It runs inside the run, after the run's execution context
	// exists, so its failure is a setup failure under the run's failure policy.
	// The request arrives as an argument rather than through the closure: an
	// entry closes over the spec it built, which predates the driver's wiring.
	Setup func(ctx context.Context, req ChatRequest) (runPlan, error)
	// Delivery is the caller's end of the run's events.
	Delivery RunDelivery
	// Input says who holds the run's input, which is the one fact that decides
	// whether a failure leaves a user message and a failure row.
	Input inputOwnership
	// Replacement is set for retry and edit. Its presence is also what makes
	// the ending a replacement turn rather than a new one: the turn it replaces
	// is already in history, so no new user row is written.
	Replacement *turnReplacement
	// Publisher overrides the session projection publisher; a decision
	// continuation also writes the answering caller's output.
	Publisher runPublisher
	// Unattended is set by the schedule entry: nobody can answer a decision for
	// this run.
	Unattended bool
}

// admittedRun is the durable identity of the run the driver executes.
type admittedRun struct {
	Handle       sessionruntime.RunHandle
	TurnID       string
	TurnPosition *int64
	// Mode is the session mode the run was admitted in; the result record reads
	// whether a user waits for it.
	Mode string
}

// turnReplacement is what a retry or an edit adds to a run.
type turnReplacement struct {
	// Preflight runs after the workspace target is prepared, before the hook.
	Preflight func(ctx context.Context) error
	// PostPersist rewrites the replaced turn after the ending's history write
	// when no queue coordinator finalizes it.
	PostPersist func(ctx context.Context, persisted []messagepkg.Message) error
}

// runPlan is what Setup resolved.
type runPlan struct {
	Exec runExecutor
	// Req carries RunID, Query and RawQuery as Setup resolved them.
	Req ChatRequest
	// IdleEffort is the reasoning effort the idle watchdog scales by.
	IdleEffort string
	// Lifecycle writes the run's context lifecycle terminal once with the run's
	// cause; nil when the plan has none. It carries the context the run
	// assembled, which the minimal staging placeholder does not.
	Lifecycle func(cause error)
}

// ReqFor is the request the driver executes with: the request Setup resolved,
// which carries the identity, route and turn the run was admitted for. A plan
// that resolved nothing leaves the entry's request as it is.
func (p runPlan) ReqFor(req ChatRequest) ChatRequest {
	if p.Req.BotID == "" {
		return req
	}
	return p.Req
}

// runResult is how the run ended, for the entry that started it.
type runResult struct {
	End       runEnd
	Failure   *RunFailure                // set when End == endFailed
	Terminal  sessionruntime.TerminalRun // the record the terminal write produced; zero when none
	Summary   commitSummary              // final text, usage, model id
	Persisted []messagepkg.Message       // rows the ending's history write produced
}
