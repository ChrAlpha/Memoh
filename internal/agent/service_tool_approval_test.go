package agent

import (
	"context"
	"testing"

	"github.com/felinics/memoh/internal/agent/decide"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
)

// A live management approval is consumed by the call that prepared it, so
// answering it never replays the tool.
func TestCapabilityApprovalDoesNotReplay(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"mcp_manage", "app_manage"} {
		t.Run(name, func(t *testing.T) {
			service := newTestService(t, Deps{})
			target := toolapproval.Request{ID: "approval-1", BotID: "bot-1", ToolName: name, Operation: toolapproval.OperationPermission, Status: toolapproval.StatusApproved}
			// No agent is configured: a replay would fail instead of acknowledging
			// the decision consumed by the already-running prepared operation.
			committed := decide.NewCommittedToolApproval(target, decide.ToolApprovalResponseInput{}, true)
			if err := service.ContinueCommittedToolApprovalResponse(context.Background(), committed, nil); err != nil {
				t.Fatalf("live management approval must not replay: %v", err)
			}
		})
	}
}
