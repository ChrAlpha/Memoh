package tools

import (
	"context"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
)

// SubagentTerminal is the terminal audit data returned to the application
// boundary after the subagent's internal retry loop has ended.
type SubagentTerminal struct {
	Cause            error
	ContextLifecycle *contextfrag.LifecycleSnapshot
	OutcomeResolved  bool
	Outcome          SpawnAttemptDisposition
}

// SubagentAdmitter is the durable admission gate a spawned agent's turn passes
// through before it executes.
//
// A subagent occupies a thread exactly like any other turn, so it takes the
// same durable slot on the same terms: the run is recorded before anything
// runs, its owner holds a fencing token, and the returned finish closes the
// record. Declaring the port here rather than importing the runtime keeps the
// dependency pointing the one way it can — the runtime is built on top of the
// agent, which is built on top of these tools.
type SubagentAdmitter interface {
	// AdmitSubagentRun returns the context the run must execute in, the
	// admitted identity, and the terminal write that releases the thread's
	// slot, or an error naming why nothing was started. turn.ErrSessionBusy
	// means the agent is already working; turn.ErrDuplicateTurn means this
	// task already has a run.
	AdmitSubagentRun(ctx context.Context, botID, threadID, invocationID string, submission []byte) (context.Context, SubagentAdmission, func(SubagentTerminal), error)
}

// SubagentAdmission names what admission allocated for the run. The turn
// identity matters to persistence: the task's user message must file as that
// turn's request (SR-TURN-001) so history and the live runtime view agree on
// which turn the run writes into — minting a second turn here would split one
// run across two turns in every subscriber's transcript.
type SubagentAdmission struct {
	RunID        string
	TurnID       string
	TurnPosition int64
}
