package native

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/jsonschema-go/jsonschema"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/step"
	agenttools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
)

// contextSwitchRun drives a tool loop whose results outgrow a 3900-token
// allowance from the fourth call on. Each call before final asks for one more
// lookup; switch answers every context switch.
type contextSwitchRun struct {
	result int
	// inject arrives while the third call runs, at the boundary the first
	// switch happens on.
	inject    string
	injectCh  chan InjectMessage
	mu        sync.Mutex
	requests  []sdk.Request
	switches  []ContextSwitchInput
	committed []int
	carried   map[int]string
	ledger    *contextfrag.MutationLedger
}

func (r *contextSwitchRun) run(t *testing.T, streaming bool, calls int, switchFn func(int, ContextSwitchInput) (sdk.Message, error)) error {
	t.Helper()
	lookup := toolexec.Tool{
		Name:       "lookup",
		Parameters: &jsonschema.Schema{Type: "object"},
		Execute: func(_ *toolexec.ToolExecContext, _ sdk.ToolArguments) (sdk.ToolOutput, error) {
			return toolexec.OutputFromValue(strings.Repeat("r", max(r.result, 6000))), nil
		},
	}
	provider := &atomicMockProvider{handler: func(call int, params sdk.Request) (sdk.ModelResult, error) {
		r.mu.Lock()
		r.requests = append(r.requests, params)
		r.mu.Unlock()
		if call == 3 && r.injectCh != nil {
			r.injectCh <- InjectMessage{Text: r.inject}
		}
		if call >= calls {
			return sdk.ModelResult{Text: "done", FinishReason: sdk.FinishReasonStop}, nil
		}
		return sdk.ModelResult{FinishReason: sdk.FinishReasonToolCalls, ToolCalls: []sdk.ToolCall{{
			ToolCallID: fmt.Sprintf("call-%d", call), ToolName: "lookup", Input: toolexec.ArgumentsFromValue(map[string]any{}),
		}}}, nil
	}}
	a := New(Deps{ContextViewApplier: func(_ context.Context, cfg RunConfig) (RunConfig, error) { return cfg, nil }})
	a.SetToolProviders([]agenttools.ToolProvider{staticToolProvider{tools: []toolexec.Tool{lookup}}})
	plan := contextfrag.ContextBudgetPlan{Window: 4000, OutputReserve: 100}
	r.ledger = contextfrag.NewMutationLedger()
	cfg := RunConfig{
		Model:            &sdk.Model{ID: "mock-model", Provider: provider},
		System:           "system",
		Messages:         []sdk.Message{sdk.UserMessage("task")},
		SupportsToolCall: true,
		Identity:         SessionContext{BotID: "bot-1"},
		ContextMutations: r.ledger,
		ContextManifest:  contextfrag.Manifest{BudgetPlan: &plan},
		OnStepCommitted: func(_ context.Context, index int, record *step.Record) (StepDirective, error) {
			r.mu.Lock()
			r.committed = append(r.committed, index)
			if r.carried == nil {
				r.carried = map[int]string{}
			}
			r.carried[index] = requestText(sdk.Request{Messages: record.Messages})
			r.mu.Unlock()
			return StepDirective{}, nil
		},
		SwitchContext: func(_ context.Context, input ContextSwitchInput) (sdk.Message, error) {
			r.switches = append(r.switches, input)
			return switchFn(len(r.switches), input)
		},
	}
	if !streaming {
		_, err := a.Generate(t.Context(), cfg)
		return err
	}
	r.injectCh = make(chan InjectMessage, 1)
	cfg.InjectCh = r.injectCh
	var err error
	for event := range a.Stream(t.Context(), cfg) {
		if event.Type == EventError && err == nil {
			err = errors.New(event.Error)
		}
	}
	return err
}

func requestText(request sdk.Request) string {
	var sb strings.Builder
	for _, message := range request.Messages {
		for _, part := range message.Content {
			switch p := part.(type) {
			case sdk.TextPart:
				sb.WriteString(p.Text)
			case sdk.ToolCallPart:
				sb.WriteString(p.ToolCallID)
			case sdk.ToolResultPart:
				sb.WriteString(p.ToolCallID)
			}
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

func TestContextSwitchFoldsEarlierStepsAndKeepsTheLatestVerbatim(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%v", streaming), func(t *testing.T) {
			r := contextSwitchRun{inject: "steer while folding"}
			err := r.run(t, streaming, 6, func(n int, _ ContextSwitchInput) (sdk.Message, error) {
				return sdk.UserMessage(fmt.Sprintf("summary-%d", n)), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(r.switches) != 2 || r.switches[0].LatestStep != 2 || r.switches[1].LatestStep != 4 {
				t.Fatalf("switches=%+v, want one at step 3 keeping step 2 and one at step 5 keeping step 4", r.switches)
			}
			for _, in := range r.switches {
				if in.KeptTokens <= 0 || in.KeptTokens >= in.AllowanceTokens || in.AllowanceTokens != 3900 {
					t.Fatalf("switch input=%+v", in)
				}
			}
			for i, want := range map[int]struct{ has, lacks []string }{
				3: {has: []string{"task", "summary-1", "call-3"}, lacks: []string{"call-1", "call-2"}},
				5: {has: []string{"task", "summary-2", "call-5"}, lacks: []string{"summary-1", "call-3", "call-4"}},
			} {
				text := requestText(r.requests[i])
				for _, s := range want.has {
					if !strings.Contains(text, s) {
						t.Fatalf("request %d lacks %q:\n%s", i+1, s, text)
					}
				}
				for _, s := range want.lacks {
					if strings.Contains(text, s) {
						t.Fatalf("request %d still carries %q:\n%s", i+1, s, text)
					}
				}
			}
			if fmt.Sprint(r.committed) != "[0 1 2 3 4 5]" {
				t.Fatalf("committed=%v", r.committed)
			}
			recorded := 0
			for _, mutation := range r.ledger.Records() {
				if mutation.Kind == contextfrag.MutationContextSwitch {
					recorded++
				}
			}
			if recorded != 2 {
				t.Fatalf("context_switch mutations=%d, want 2", recorded)
			}
			if streaming && (!strings.Contains(requestText(r.requests[3]), r.inject) || !strings.Contains(r.carried[3], r.inject)) {
				t.Fatalf("input injected at the switch boundary was not sent and stored with step 3: stored=%q", r.carried[3])
			}
		})
	}
}

func TestContextSwitchFailsTheRunWhenItCannotHelp(t *testing.T) {
	for _, tc := range []struct {
		name     string
		result   int
		switches int
		summary  func(int, ContextSwitchInput) (sdk.Message, error)
	}{
		{name: "summarizer failed", switches: 1, summary: func(int, ContextSwitchInput) (sdk.Message, error) {
			return sdk.Message{}, fmt.Errorf("%w: summarizer down", contextfrag.ErrBudgetUnsatisfied)
		}},
		{name: "empty summary", switches: 1, summary: func(int, ContextSwitchInput) (sdk.Message, error) {
			return sdk.UserMessage(" "), nil
		}},
		{name: "summary does not fit", switches: 1, summary: func(int, ContextSwitchInput) (sdk.Message, error) {
			return sdk.UserMessage(strings.Repeat("s", 12000)), nil
		}},
		{name: "only the latest step", result: 20000, summary: func(int, ContextSwitchInput) (sdk.Message, error) {
			return sdk.UserMessage("unused"), nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := contextSwitchRun{result: tc.result}
			err := r.run(t, false, 6, tc.summary)
			if !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) || len(r.switches) != tc.switches {
				t.Fatalf("err=%v switches=%d, want budget_unsatisfied after %d switches", err, len(r.switches), tc.switches)
			}
			want := "[0 1 2]"
			if tc.switches == 0 {
				want = "[0]"
			}
			if fmt.Sprint(r.committed) != want || len(r.requests) != len(r.committed) {
				t.Fatalf("requests=%d committed=%v, want no call after the failure and the committed steps kept", len(r.requests), r.committed)
			}
		})
	}
}
