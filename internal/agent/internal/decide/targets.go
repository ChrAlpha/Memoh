package decide

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// ResolveRuntimeDecision reads the authoritative decision row before any live
// owner lookup. Terminal rows are returned too: the router needs to distinguish
// a known, already-decided request from an unfenced ACP/MCP request.
func (d *Decider) ResolveRuntimeDecision(ctx context.Context, commandType, decisionID string) (sessionruntime.DecisionTarget, error) {
	id, err := db.ParseUUID(decisionID)
	if err != nil {
		return sessionruntime.DecisionTarget{}, fmt.Errorf("%w: %w", sessionruntime.ErrDecisionNotFound, err)
	}
	switch commandType {
	case sessionruntime.CommandToolApprovalResponse:
		row, err := d.queries.GetToolApprovalRequest(ctx, id)
		if err != nil {
			return sessionruntime.DecisionTarget{}, runtimeDecisionReadError(err)
		}
		return toolApprovalDecisionTarget(row), nil
	case sessionruntime.CommandUserInputResponse:
		row, err := d.queries.GetUserInputRequest(ctx, id)
		if err != nil {
			return sessionruntime.DecisionTarget{}, runtimeDecisionReadError(err)
		}
		return userInputDecisionTarget(row), nil
	default:
		return sessionruntime.DecisionTarget{}, fmt.Errorf("unsupported runtime decision command %q", commandType)
	}
}

// PendingRuntimeDecisions resolves every durable decision that parked runID.
// It is used only by expired-owner recovery, where preserving the exact rows
// is required before advancing the run's fencing token — a turn can park on
// several approvals and user inputs at once, and dropping any of them here
// would supersede a decision the user can still answer.
func (d *Decider) PendingRuntimeDecisions(ctx context.Context, runID string) ([]sessionruntime.DecisionTarget, error) {
	id, err := db.ParseUUID(runID)
	if err != nil {
		return nil, err
	}
	approvals, err := d.queries.ListPendingToolApprovalsByRun(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read pending tool approvals for run: %w", err)
	}
	inputs, err := d.queries.ListPendingUserInputsByRun(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read pending user inputs for run: %w", err)
	}
	out := make([]sessionruntime.DecisionTarget, 0, len(approvals)+len(inputs))
	for _, approval := range approvals {
		out = append(out, toolApprovalDecisionTarget(approval))
	}
	for _, input := range inputs {
		out = append(out, userInputDecisionTarget(input))
	}
	// Recovery decides by session runtime whether the parked run is
	// resumable at all; every pending decision here shares one session.
	if len(out) > 0 {
		sess, sessErr := d.sessions.Get(ctx, out[0].SessionID)
		if sessErr != nil {
			return nil, fmt.Errorf("read waiting-decision session runtime: %w", sessErr)
		}
		for i := range out {
			out[i].SessionRuntime = sess.RuntimeType
		}
	}
	return out, nil
}

func runtimeDecisionReadError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return sessionruntime.ErrDecisionNotFound
	}
	return err
}

func toolApprovalDecisionTarget(row dbsqlc.ToolApprovalRequest) sessionruntime.DecisionTarget {
	return sessionruntime.DecisionTarget{
		Type: sessionruntime.CommandToolApprovalResponse, ID: runreq.PGUUIDString(row.ID),
		BotID: runreq.PGUUIDString(row.BotID), SessionID: runreq.PGUUIDString(row.SessionID),
		RunID: runreq.PGUUIDString(row.RunID), TurnID: runreq.PGUUIDString(row.TurnID),
		Status: row.Status, FencingToken: runreq.PGInt64(row.RuntimeFencingToken),
		InlineDecision: isCapabilityManagementApproval(toolapproval.Request{ToolName: row.ToolName}),
		ControlID:      db.TextToString(row.ResponseControlID), PayloadHash: db.TextToString(row.ResponsePayloadHash),
	}
}

func userInputDecisionTarget(row dbsqlc.UserInputRequest) sessionruntime.DecisionTarget {
	return sessionruntime.DecisionTarget{
		Type: sessionruntime.CommandUserInputResponse, ID: runreq.PGUUIDString(row.ID),
		BotID: runreq.PGUUIDString(row.BotID), SessionID: runreq.PGUUIDString(row.SessionID),
		RunID: runreq.PGUUIDString(row.RunID), TurnID: runreq.PGUUIDString(row.TurnID),
		Status: row.Status, FencingToken: runreq.PGInt64(row.RuntimeFencingToken),
		ControlID: db.TextToString(row.ResponseControlID), PayloadHash: db.TextToString(row.ResponsePayloadHash),
	}
}
