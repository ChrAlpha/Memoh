package contextview

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/jsonschema-go/jsonschema"

	agentpkg "github.com/felinics/memoh/internal/agent/runtime/native"
	agenttools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
)

// A long tool loop that is not compacted, or whose compaction gives nothing
// back, sends every loop message to the provider as it is: each request is the
// previous one plus the step after it, and every tool result is whole.
func TestLongToolLoopSendsEveryLoopMessageAsItIs(t *testing.T) {
	t.Parallel()

	const toolSteps = 14
	result := func(step int) string {
		return fmt.Sprintf("result-%02d ", step) + strings.Repeat("x", 2_000)
	}
	for _, tc := range []struct {
		name      string
		compactor agentpkg.StepContextCompactor
	}{
		{name: "no compaction"},
		{name: "compaction gives nothing back", compactor: func(context.Context, agentpkg.StepContextInput) []sdk.Message { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			var sent []sdk.Request
			provider := &envelopeProbeProvider{handler: func(call int, params sdk.Request) (sdk.ModelResult, error) {
				mu.Lock()
				sent = append(sent, params)
				mu.Unlock()
				if call > toolSteps {
					return sdk.ModelResult{Text: "done", FinishReason: sdk.FinishReasonStop}, nil
				}
				return sdk.ModelResult{FinishReason: sdk.FinishReasonToolCalls, ToolCalls: []sdk.ToolCall{{
					ToolCallID: fmt.Sprintf("call-%02d", call), ToolName: "lookup",
					Input: toolexec.ArgumentsFromValue(map[string]any{"step": call}),
				}}}, nil
			}}
			var step int
			agent := agentpkg.New(agentpkg.Deps{ContextViewApplier: ProviderRunConfigApplier(nil)})
			agent.SetToolProviders([]agenttools.ToolProvider{envelopeProbeToolProvider{tools: []toolexec.Tool{{
				Name:       "lookup",
				Parameters: &jsonschema.Schema{Type: "object"},
				Execute: func(*toolexec.ToolExecContext, sdk.ToolArguments) (sdk.ToolOutput, error) {
					step++
					return toolexec.OutputFromValue(result(step)), nil
				},
			}}}})
			currentIndex := 0
			if _, err := agent.Generate(context.Background(), agentpkg.RunConfig{
				Model:                          &sdk.Model{ID: "model", Provider: provider, Type: sdk.ModelTypeChat},
				System:                         "you are helpful",
				Messages:                       []sdk.Message{sdk.UserMessage("research this")},
				ContextCurrentUserMessageIndex: &currentIndex,
				ContextBudgetMaxTokens:         200_000,
				SupportsToolCall:               true,
				Identity:                       agentpkg.SessionContext{BotID: "bot-1"},
				StepContextCompactor:           tc.compactor,
			}); err != nil {
				t.Fatalf("Generate() error = %v", err)
			}

			if len(sent) != toolSteps+1 {
				t.Fatalf("provider calls = %d, want %d", len(sent), toolSteps+1)
			}
			for call := 1; call < len(sent); call++ {
				prev, next := sent[call-1].Messages, sent[call].Messages
				if len(next) != len(prev)+2 {
					t.Fatalf("call %d sent %d messages, want the %d of call %d plus one step", call+1, len(next), len(prev), call)
				}
				if got, want := marshalMessages(t, next[:len(prev)]), marshalMessages(t, prev); got != want {
					t.Fatalf("call %d rewrote the messages call %d sent", call+1, call)
				}
			}
			last := marshalMessages(t, sent[toolSteps].Messages)
			for s := 1; s <= toolSteps; s++ {
				if !strings.Contains(last, result(s)) {
					t.Fatalf("the last request lost the whole result of step %d", s)
				}
			}
			if strings.Contains(last, "pruned") {
				t.Fatal("the last request carries a stubbed tool result")
			}
		})
	}
}

func marshalMessages(t *testing.T, messages []sdk.Message) string {
	t.Helper()
	raw, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	return string(raw)
}
