package codex

import (
	"context"
	"strings"

	"github.com/felinics/memoh/internal/agent/runtime/agentprocess"
	"github.com/felinics/memoh/internal/agent/runtime/external"
)

func (*Driver) SubagentCapabilities() external.SubagentCapabilities {
	return external.SubagentCapabilities{IsolatedExecution: true, NoNestedDelegation: true, EarlyRuntimeAnchor: true}
}

func promptServerKey(input external.PromptInput) string {
	key := serverKey(input.BotID, input.BotAgentID)
	if input.IsSubagent() {
		key += "\x00" + input.ThreadID + ":" + input.RunID
	}
	return key
}

func scopedServerContext(ctx context.Context, key string) context.Context {
	parts := strings.SplitN(key, "\x00", 3)
	if len(parts) < 3 {
		return ctx
	}
	child, run, _ := strings.Cut(parts[2], ":")
	return agentprocess.WithExecutionScope(ctx, agentprocess.ExecutionScope{ThreadID: child, RunID: run})
}

func (d *Driver) ReconcileSubagent(ctx context.Context, input external.PromptInput) error {
	if !input.IsSubagent() {
		return external.ErrControlUnsupported
	}
	d.servers.Recycle(promptServerKey(input))
	client, err := d.bridges.MCPClient(ctx, input.BotID)
	if err != nil {
		return err
	}
	return (agentprocess.ExecutionScope{ThreadID: input.ThreadID, RunID: input.RunID}).Reconcile(ctx, client)
}
