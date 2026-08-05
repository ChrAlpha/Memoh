package native

import (
	"context"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	agenttools "github.com/memohai/memoh/internal/agent/tool"
)

func TestAgentStreamRecordsInjectedMessageMutation(t *testing.T) {
	t.Parallel()

	const marker = "injected between provider steps"
	injectCh := make(chan InjectMessage, 1)
	injectCh <- InjectMessage{Text: marker}

	var secondCall sdk.GenerateParams
	provider := &atomicMockProvider{handler: func(call int, params sdk.GenerateParams) (*sdk.GenerateResult, error) {
		if call == 1 {
			return &sdk.GenerateResult{
				FinishReason: sdk.FinishReasonToolCalls,
				ToolCalls:    []sdk.ToolCall{{ToolCallID: "inject-call", ToolName: "lookup"}},
			}, nil
		}
		secondCall = cloneGenerateParams(params)
		return &sdk.GenerateResult{Text: "done", FinishReason: sdk.FinishReasonStop}, nil
	}}
	a := New(Deps{})
	a.SetToolProviders([]agenttools.ToolProvider{staticToolProvider{tools: []sdk.Tool{{
		Name:       "lookup",
		Parameters: &jsonschema.Schema{Type: "object"},
		Execute: func(*sdk.ToolExecContext, any) (any, error) {
			return map[string]any{"ok": true}, nil
		},
	}}}})
	ledger := contextfrag.NewMutationLedger()

	for range a.Stream(context.Background(), RunConfig{
		Model:            &sdk.Model{ID: "mock-model", Provider: provider},
		Messages:         []sdk.Message{sdk.UserMessage("start")},
		SupportsToolCall: true,
		InjectCh:         injectCh,
		Identity:         SessionContext{BotID: "bot-1"},
		ContextMutations: ledger,
	}) {
	}
	if !providerAttemptContainsText(secondCall.Messages, marker) {
		t.Fatalf("second provider call lost injected message: %#v", secondCall.Messages)
	}
	if !hasMutationKind(ledger.Records(), contextfrag.MutationInjectedMessage) {
		t.Fatalf("mutations = %#v, want %q", ledger.Records(), contextfrag.MutationInjectedMessage)
	}
}
