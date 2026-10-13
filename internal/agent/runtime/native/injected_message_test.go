package native

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/jsonschema-go/jsonschema"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	agenttools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
)

func TestAgentStreamRecordsInjectedMessageMutation(t *testing.T) {
	t.Parallel()

	const marker = "injected between provider steps"
	injectCh := make(chan InjectMessage, 1)
	injectCh <- InjectMessage{Text: marker}

	var secondCall sdk.Request
	provider := &atomicMockProvider{handler: func(call int, params sdk.Request) (sdk.ModelResult, error) {
		if call == 1 {
			return sdk.ModelResult{
				FinishReason: sdk.FinishReasonToolCalls,
				ToolCalls:    []sdk.ToolCall{{ToolCallID: "inject-call", ToolName: "lookup"}},
			}, nil
		}
		secondCall = cloneGenerateParams(params)
		return sdk.ModelResult{Text: "done", FinishReason: sdk.FinishReasonStop}, nil
	}}
	a := New(Deps{})
	a.SetToolProviders([]agenttools.ToolProvider{staticToolProvider{tools: []toolexec.Tool{{
		Name:       "lookup",
		Parameters: &jsonschema.Schema{Type: "object"},
		Execute: func(*toolexec.ToolExecContext, sdk.ToolArguments) (sdk.ToolOutput, error) {
			return toolexec.OutputFromValue(map[string]any{"ok": true}), nil
		},
	}}}})
	ledger := contextfrag.NewMutationLedger()
	type recordedInjection struct {
		text        string
		insertAfter int
	}
	var recorded []recordedInjection

	var terminal StreamEvent
	for event := range a.Stream(context.Background(), RunConfig{
		Model:            &sdk.Model{ID: "mock-model", Provider: provider},
		Messages:         []sdk.Message{sdk.UserMessage("start")},
		SupportsToolCall: true,
		InjectCh:         injectCh,
		Identity:         SessionContext{BotID: "bot-1"},
		ContextMutations: ledger,
		InjectedRecorder: func(text string, insertAfter int) {
			recorded = append(recorded, recordedInjection{text: text, insertAfter: insertAfter})
		},
	}) {
		if event.IsTerminal() {
			terminal = event
			if len(recorded) != 1 {
				t.Fatalf("recorded injections at terminal delivery = %#v, want one", recorded)
			}
		}
	}
	if !providerAttemptContainsText(secondCall.Messages, marker) {
		t.Fatalf("second provider call lost injected message: %#v", secondCall.Messages)
	}
	if !hasMutationKind(ledger.Records(), contextfrag.MutationInjectedMessage) {
		t.Fatalf("mutations = %#v, want %q", ledger.Records(), contextfrag.MutationInjectedMessage)
	}
	if terminal.Type != EventAgentEnd {
		t.Fatalf("terminal event = %q, want %q", terminal.Type, EventAgentEnd)
	}
	if len(recorded) != 1 || recorded[0].text != marker {
		t.Fatalf("recorded injections = %#v, want marker once", recorded)
	}
}

func TestAgentStreamRecordsDuplicateAdmittedInjections(t *testing.T) {
	t.Parallel()

	const marker = "duplicate admitted injection"
	injectCh := make(chan InjectMessage, 2)
	injectCh <- InjectMessage{Text: marker}
	injectCh <- InjectMessage{Text: marker}

	provider := &atomicMockProvider{handler: func(call int, params sdk.Request) (sdk.ModelResult, error) {
		if call == 1 {
			return sdk.ModelResult{
				FinishReason: sdk.FinishReasonToolCalls,
				ToolCalls:    []sdk.ToolCall{{ToolCallID: "duplicate-inject", ToolName: "lookup"}},
			}, nil
		}
		raw, err := json.Marshal(params.Messages)
		if err != nil {
			t.Fatalf("marshal provider messages: %v", err)
		}
		if got := strings.Count(string(raw), marker); got != 2 {
			t.Fatalf("provider injection count = %d, want 2: %s", got, raw)
		}
		return sdk.ModelResult{Text: "done", FinishReason: sdk.FinishReasonStop}, nil
	}}
	a := New(Deps{})
	a.SetToolProviders([]agenttools.ToolProvider{staticToolProvider{tools: []toolexec.Tool{{
		Name:       "lookup",
		Parameters: &jsonschema.Schema{Type: "object"},
		Execute: func(*toolexec.ToolExecContext, sdk.ToolArguments) (sdk.ToolOutput, error) {
			return toolexec.OutputFromValue(map[string]any{"ok": true}), nil
		},
	}}}})

	type recordedInjection struct {
		text        string
		insertAfter int
	}
	var recorded []recordedInjection
	for range a.Stream(context.Background(), RunConfig{
		Model:            &sdk.Model{ID: "mock-model", Provider: provider},
		Messages:         []sdk.Message{sdk.UserMessage("start")},
		SupportsToolCall: true,
		InjectCh:         injectCh,
		Identity:         SessionContext{BotID: "bot-1"},
		ContextMutations: contextfrag.NewMutationLedger(),
		InjectedRecorder: func(text string, insertAfter int) {
			recorded = append(recorded, recordedInjection{text: text, insertAfter: insertAfter})
		},
	}) {
	}

	if len(recorded) != 2 || recorded[0].text != marker || recorded[1].text != marker {
		t.Fatalf("recorded injections = %#v, want duplicate marker in arrival order", recorded)
	}
	if recorded[0].insertAfter != recorded[1].insertAfter {
		t.Fatalf("recorded insertion boundaries = %d/%d, want identical boundary for one PrepareStep", recorded[0].insertAfter, recorded[1].insertAfter)
	}
}

func TestAgentStreamRecordsLaterInjectionAtOutputBoundary(t *testing.T) {
	t.Parallel()

	const firstMarker = "first boundary injection"
	const secondMarker = "second boundary injection"
	injectCh := make(chan InjectMessage, 2)
	injectCh <- InjectMessage{Text: firstMarker}

	provider := &atomicMockProvider{handler: func(call int, params sdk.Request) (sdk.ModelResult, error) {
		switch call {
		case 1:
			return sdk.ModelResult{
				FinishReason: sdk.FinishReasonToolCalls,
				ToolCalls:    []sdk.ToolCall{{ToolCallID: "boundary-one", ToolName: "lookup"}},
			}, nil
		case 2:
			if !providerAttemptContainsText(params.Messages, firstMarker) {
				t.Fatal("second provider call lost first boundary injection")
			}
			injectCh <- InjectMessage{Text: secondMarker}
			return sdk.ModelResult{
				FinishReason: sdk.FinishReasonToolCalls,
				ToolCalls:    []sdk.ToolCall{{ToolCallID: "boundary-two", ToolName: "lookup"}},
			}, nil
		default:
			if !providerAttemptContainsText(params.Messages, secondMarker) {
				t.Fatal("third provider call lost second boundary injection")
			}
			return sdk.ModelResult{Text: "done", FinishReason: sdk.FinishReasonStop}, nil
		}
	}}
	a := New(Deps{})
	a.SetToolProviders([]agenttools.ToolProvider{staticToolProvider{tools: []toolexec.Tool{{
		Name:       "lookup",
		Parameters: &jsonschema.Schema{Type: "object"},
		Execute: func(*toolexec.ToolExecContext, sdk.ToolArguments) (sdk.ToolOutput, error) {
			return toolexec.OutputFromValue(map[string]any{"ok": true}), nil
		},
	}}}})

	type recordedInjection struct {
		text        string
		insertAfter int
	}
	var recorded []recordedInjection
	for range a.Stream(context.Background(), RunConfig{
		Model:            &sdk.Model{ID: "mock-model", Provider: provider},
		Messages:         []sdk.Message{sdk.UserMessage("start")},
		SupportsToolCall: true,
		InjectCh:         injectCh,
		Identity:         SessionContext{BotID: "bot-1"},
		ContextMutations: contextfrag.NewMutationLedger(),
		InjectedRecorder: func(text string, insertAfter int) {
			recorded = append(recorded, recordedInjection{text: text, insertAfter: insertAfter})
		},
	}) {
	}

	if len(recorded) != 2 {
		t.Fatalf("recorded injections = %#v, want two", recorded)
	}
	if recorded[0] != (recordedInjection{text: firstMarker, insertAfter: 2}) {
		t.Fatalf("first injection = %#v, want boundary after first tool pair", recorded[0])
	}
	if recorded[1] != (recordedInjection{text: secondMarker, insertAfter: 4}) {
		t.Fatalf("second injection = %#v, want boundary after two tool pairs", recorded[1])
	}
}
