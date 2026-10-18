package native

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/jsonschema-go/jsonschema"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/step"
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

// A retryable failure on the attempt that carried the injected message is
// retried with the message still in the input, once: the recorder and the
// terminal messages see a single copy.
func TestAgentStreamRetryAfterRetryableFailureKeepsInjectedMessageOnce(t *testing.T) {
	t.Parallel()

	const marker = "retried injection"
	injectCh := make(chan InjectMessage, 1)
	injectCh <- InjectMessage{Text: marker}

	var attemptCarriers []int
	provider := &atomicMockProvider{handler: func(call int, params sdk.Request) (sdk.ModelResult, error) {
		switch call {
		case 1:
			return sdk.ModelResult{
				FinishReason: sdk.FinishReasonToolCalls,
				ToolCalls:    []sdk.ToolCall{{ToolCallID: "retry-inject", ToolName: "lookup"}},
			}, nil
		case 2:
			attemptCarriers = append(attemptCarriers, countTextParts(params.Messages, marker))
			return sdk.ModelResult{}, serverErr()
		default:
			attemptCarriers = append(attemptCarriers, countTextParts(params.Messages, marker))
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

	var recorded []string
	var committed []step.Record
	var terminal StreamEvent
	for event := range a.Stream(context.Background(), RunConfig{
		Model:            &sdk.Model{ID: "mock-model", Provider: provider},
		Messages:         []sdk.Message{sdk.UserMessage("start")},
		SupportsToolCall: true,
		InjectCh:         injectCh,
		Identity:         SessionContext{BotID: "bot-1"},
		ContextMutations: contextfrag.NewMutationLedger(),
		InjectedRecorder: func(text string, _ int) { recorded = append(recorded, text) },
		OnStepCommitted: func(_ context.Context, _ int, record *step.Record) (StepDirective, error) {
			committed = append(committed, *record)
			return StepDirective{}, nil
		},
	}) {
		if event.IsTerminal() {
			terminal = event
		}
	}

	if provider.calls.Load() != 3 {
		t.Fatalf("provider calls = %d, want 3", provider.calls.Load())
	}
	if len(attemptCarriers) != 2 || attemptCarriers[0] != 1 || attemptCarriers[1] != 1 {
		t.Fatalf("injection copies per attempt = %v, want [1 1]", attemptCarriers)
	}
	if terminal.Type != EventAgentEnd {
		t.Fatalf("terminal event = %q, want %q", terminal.Type, EventAgentEnd)
	}
	if len(recorded) != 1 || recorded[0] != marker {
		t.Fatalf("recorded injections = %#v, want marker once", recorded)
	}
	copies := 0
	for _, record := range committed {
		copies += countTextParts(record.Messages, marker)
	}
	if copies != 1 {
		t.Fatalf("committed steps carry the injection %d times, want once", copies)
	}
}

func countTextParts(messages []sdk.Message, text string) int {
	count := 0
	for _, msg := range messages {
		for _, part := range msg.Content {
			if value, ok := part.(sdk.TextPart); ok && strings.Contains(value.Text, text) {
				count++
			}
		}
	}
	return count
}

// A ref counts as carried only when the message at its index is the tracked
// one; anything that cannot be located is treated as not carried.
func TestVerifyDynamicRefsRequiresTrackedMessageAtIndex(t *testing.T) {
	t.Parallel()

	injected := sdk.UserMessage("second suffix")
	input := []sdk.Message{sdk.UserMessage("fixed prefix"), sdk.AssistantMessage("first suffix"), injected}
	cached := cloneProviderMessages(input)
	part := cached[2].Content[0].(sdk.TextPart)
	part.CacheControl = &sdk.CacheControl{Type: "ephemeral"}
	cached[2].Content[0] = part

	dynamic := newLoopDynamicInputs(0)
	dynamic.append(injected, false, "second suffix", 2)
	ref := dynamicSourceRef{recordID: 0, index: 2}

	tests := []struct {
		name     string
		dynamic  *loopDynamicInputs
		refs     []dynamicSourceRef
		messages []sdk.Message
		want     []dynamicSourceRef
	}{
		{"unchanged", dynamic, []dynamicSourceRef{ref}, input, []dynamicSourceRef{ref}},
		{"cache control only", dynamic, []dynamicSourceRef{ref}, cached, []dynamicSourceRef{ref}},
		{"content mismatch", dynamic, []dynamicSourceRef{ref}, []sdk.Message{input[0], input[1], sdk.UserMessage("rewritten")}, nil},
		{"index past the input", dynamic, []dynamicSourceRef{ref}, input[:2], nil},
		{"negative index", dynamic, []dynamicSourceRef{{recordID: 0, index: -1}}, input, nil},
		{"unknown record", dynamic, []dynamicSourceRef{{recordID: 9, index: 2}}, input, nil},
		{"no refs", dynamic, nil, input, nil},
		{"no tracker", nil, []dynamicSourceRef{ref}, input, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := verifyDynamicRefs(tt.dynamic, tt.refs, tt.messages)
			if len(got)+len(tt.want) > 0 && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("verified refs = %#v, want %#v", got, tt.want)
			}
		})
	}
}
