package claudecode

import (
	"context"

	"github.com/felinics/memoh/internal/agent/runtime/agentprocess"
	"github.com/felinics/memoh/internal/agent/runtime/external"
)

func (*Driver) SubagentCapabilities() external.SubagentCapabilities {
	return external.SubagentCapabilities{IsolatedExecution: true, NoNestedDelegation: true, EarlyRuntimeAnchor: true}
}

func (d *Driver) ReconcileSubagent(ctx context.Context, input external.PromptInput) error {
	if !input.IsSubagent() {
		return external.ErrControlUnsupported
	}
	client, err := d.bridges.MCPClient(ctx, input.BotID)
	if err != nil {
		return err
	}
	return (agentprocess.ExecutionScope{ThreadID: input.ThreadID, RunID: input.RunID}).Reconcile(ctx, client)
}
