package drive

import (
	"context"

	"github.com/felinics/memoh/internal/agent/agentapi"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// RunSpec is what one entry decides about a run. Everything else about running
// it is the same for every entry.
type RunSpec struct {
	// Run is the admitted run. A zero Handle (FencingToken <= 0) is a run nobody
	// admitted: the driver neither publishes it nor writes its terminal state.
	Run AdmittedRun
	// Req is the request as the entry built it: identity, route, query, turn.
	// The driver finishes the wiring the entry cannot do itself — the asset
	// collector it builds per run — and hands the result to Setup.
	Req runreq.ChatRequest
	// Setup resolves the executor from the run's request as the driver finished
	// wiring it. It runs inside the run, after the run's execution context
	// exists, so its failure is a setup failure under the run's failure policy.
	// The request arrives as an argument rather than through the closure: an
	// entry closes over the spec it built, which predates the driver's wiring.
	Setup func(ctx context.Context, req runreq.ChatRequest) (RunPlan, error)
	// Delivery is the caller's end of the run's events.
	Delivery agentapi.RunDelivery
	// Input says who holds the run's input, which is the one fact that decides
	// whether a failure leaves a user message and a failure row.
	Input outcome.InputOwnership
	// Replacement is set for retry and edit.
	Replacement *TurnReplacement
	// Publisher overrides the session projection publisher; a decision
	// continuation also writes the answering caller's output.
	Publisher RunPublisher
	// Unattended is set by the schedule entry: nobody can answer a decision for
	// this run.
	Unattended bool
}

// AdmittedRun is the durable identity of the run the driver executes.
type AdmittedRun struct {
	Handle       sessionruntime.RunHandle
	TurnID       string
	TurnPosition *int64
	// Mode is the session mode the run was admitted in; the result record reads
	// whether a user waits for it.
	Mode string
}

// TurnReplacement is what a retry or an edit adds to a run.
type TurnReplacement struct {
	// Preflight runs after the workspace target is prepared, before the hook.
	Preflight func(ctx context.Context) error
}

// RunPlan is what Setup resolved.
type RunPlan struct {
	Exec RunExecutor
	// Req carries RunID, Query and RawQuery as Setup resolved them.
	Req runreq.ChatRequest
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
func (p RunPlan) ReqFor(req runreq.ChatRequest) runreq.ChatRequest {
	if p.Req.BotID == "" {
		return req
	}
	return p.Req
}

// RunResult is how the run ended, for the entry that started it.
type RunResult struct {
	End       outcome.RunEnd
	Failure   *outcome.RunFailure        // set when End == endFailed
	Terminal  sessionruntime.TerminalRun // the record the terminal write produced; zero when none
	Summary   CommitSummary              // final text, usage, model id
	Persisted []messagepkg.Message       // rows the ending's history write produced
}
