package native

import (
	"context"
	"errors"
	"fmt"
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

type contextSwitchFixture struct {
	forks                        [][]sdk.Message
	committed, executed, adopted int
	retry                        bool
	requests                     []sdk.Request
	inputs                       []ContextSwitchInput
}

func newContextSwitchFixture(t *testing.T) (*Agent, RunConfig, *contextSwitchFixture) {
	t.Helper()
	f := &contextSwitchFixture{}
	fork := agenttools.NewMessageSnapshot(nil)
	provider := &atomicMockProvider{handler: func(call int, req sdk.Request) (sdk.ModelResult, error) {
		f.requests = append(f.requests, cloneGenerateParams(req))
		snapshot, err := fork.Messages()
		if err != nil {
			t.Error(err)
		}
		f.forks = append(f.forks, snapshot)
		if call <= 2 {
			return sdk.ModelResult{FinishReason: sdk.FinishReasonToolCalls, ToolCalls: []sdk.ToolCall{{ToolCallID: fmt.Sprint("call-", call), ToolName: "lookup"}}}, nil
		}
		if f.retry && call == 3 {
			return sdk.ModelResult{}, serverErr()
		}
		return sdk.ModelResult{Text: "done", FinishReason: sdk.FinishReasonStop}, nil
	}}
	a := New(Deps{ContextViewApplier: func(_ context.Context, cfg RunConfig) (RunConfig, error) { return cfg, nil }})
	a.SetToolProviders([]agenttools.ToolProvider{staticToolProvider{tools: []toolexec.Tool{{Name: "lookup", Parameters: &jsonschema.Schema{Type: "object"}, Execute: func(*toolexec.ToolExecContext, sdk.ToolArguments) (sdk.ToolOutput, error) {
		f.executed++
		return toolexec.OutputFromValue(strings.Repeat("fact ", 700)), nil
	}}}}})
	cfg := RunConfig{
		RunID: "same-run", Model: &sdk.Model{ID: "mock", Provider: provider}, Messages: []sdk.Message{sdk.UserMessage("current request")}, SupportsToolCall: true,
		ContextManifest: contextfrag.Manifest{BudgetPlan: &contextfrag.ContextBudgetPlan{Window: 2500}}, ContextMutations: contextfrag.NewMutationLedger(),
		ForkContext: fork, Retry: fastRetry,
		ContextStepReselector: func(context.Context, ContextStepSelectionInput) ContextStepSelectionResult {
			t.Error("context switch called legacy reselector")
			return ContextStepSelectionResult{}
		},
		OnStepCommitted: func(_ context.Context, index int, record *step.Record) (StepDirective, error) {
			if index != f.committed {
				t.Errorf("step=%d want=%d", index, f.committed)
			}
			f.committed++
			refs := make([]contextfrag.ContextRef, len(record.Messages))
			for i := range refs {
				refs[i] = contextfrag.ContextRef{Namespace: "bot_history_message", ID: fmt.Sprintf("step-%d-message-%d", index, i), Durability: contextfrag.RefDurable}
			}
			return StepDirective{CommittedSources: refs}, nil
		},
		SwitchContext: func(_ context.Context, input ContextSwitchInput) (ContextSwitchResult, error) {
			f.inputs = append(f.inputs, input)
			if input.RunID != "same-run" {
				t.Errorf("run changed: %s", input.RunID)
			}
			var result ContextSwitchResult
			summaryIndex := -1
			for i, msg := range input.Request.Messages {
				if !input.Protected[i] {
					if input.Sources[i].Ref.ID == "" {
						t.Error("unknown input is compactable")
					}
					if summaryIndex < 0 {
						summaryIndex = len(result.Messages)
						result.Messages = append(result.Messages, sdk.UserMessage("earlier work summary"))
						result.SourceIndexes = append(result.SourceIndexes, -1)
						result.Sources = append(result.Sources, ContextSwitchSource{Ref: contextfrag.ContextRef{Namespace: "compaction_summary", ID: "summary"}})
					}
					result.Sources[summaryIndex].Coverage = append(result.Sources[summaryIndex].Coverage, input.Sources[i].Ref)
					continue
				}
				result.Messages = append(result.Messages, msg)
				result.SourceIndexes = append(result.SourceIndexes, i)
				result.Sources = append(result.Sources, input.Sources[i])
			}
			result.OnAdopted = func() { f.adopted++ }
			return result, nil
		},
	}
	return a, cfg, f
}

func runContextSwitchFixture(ctx context.Context, a *Agent, cfg RunConfig, stream bool) error {
	if !stream {
		_, err := a.Generate(ctx, cfg)
		return err
	}
	var failure error
	for event := range a.Stream(ctx, cfg) {
		if event.Type == EventError {
			failure = event.Cause
		}
	}
	return failure
}

func TestContextSwitchContinuesSameRunWithCommittedSources(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, retry := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/retry=%v", stream, retry), func(t *testing.T) {
				a, cfg, f := newContextSwitchFixture(t)
				f.retry = retry
				if err := runContextSwitchFixture(context.Background(), a, cfg, stream); err != nil {
					t.Fatal(err)
				}
				if f.committed != 3 || f.executed != 2 || f.adopted != 1 {
					t.Fatalf("committed/executed/adopted=%d/%d/%d", f.committed, f.executed, f.adopted)
				}
				req := f.requests[len(f.requests)-1]
				if !providerAttemptContainsText(req.Messages, "earlier work summary") || !providerAttemptContainsText(req.Messages, "current request") {
					t.Fatal("next provider request missed adopted context")
				}
				var ids []string
				for _, msg := range req.Messages {
					for _, part := range msg.Content {
						if call, ok := part.(sdk.ToolCallPart); ok {
							ids = append(ids, call.ToolCallID)
						}
					}
				}
				if !reflect.DeepEqual(ids, []string{"call-2"}) {
					t.Fatalf("retained tool calls=%v want latest complete call-2", ids)
				}
				if len(f.inputs) != 1 || f.inputs[0].StepIndex != 2 {
					t.Fatalf("switch boundaries=%v", len(f.inputs))
				}
				input := f.inputs[0]
				for i, source := range input.Sources {
					if strings.HasPrefix(source.Ref.ID, "step-1-") && !input.Protected[i] {
						t.Fatal("latest complete interaction became compactable")
					}
				}
				if retry && len(f.requests) != 4 {
					t.Fatalf("provider calls=%d want 4", len(f.requests))
				}
			})
		}
	}
}

func TestContextSwitchRejectsProtectedLossAndNoProgress(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"protected", "source", "unchanged", "larger"} {
			t.Run(fmt.Sprintf("stream=%v/%s", stream, mode), func(t *testing.T) {
				a, cfg, f := newContextSwitchFixture(t)
				cfg.ContextManifest.BudgetPlan.Window = 1900
				original := cfg.SwitchContext
				cfg.SwitchContext = func(ctx context.Context, input ContextSwitchInput) (ContextSwitchResult, error) {
					result, err := original(ctx, input)
					switch mode {
					case "protected":
						result.Messages = result.Messages[1:]
						result.SourceIndexes = result.SourceIndexes[1:]
						result.Sources = result.Sources[1:]
					case "source":
						result.SourceIndexes[0] = len(input.Request.Messages) + 1
					case "unchanged":
						result.Messages = input.Request.Messages
						result.Sources = input.Sources
						result.SourceIndexes = make([]int, len(result.Messages))
						for i := range result.SourceIndexes {
							result.SourceIndexes[i] = i
						}
					case "larger":
						for i, source := range result.SourceIndexes {
							if source == -1 {
								result.Messages[i] = sdk.UserMessage(strings.Repeat("summary ", 1000))
							}
						}
					}
					return result, err
				}
				err := runContextSwitchFixture(context.Background(), a, cfg, stream)
				if !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) && !errors.Is(err, contextfrag.ErrProtectedContextOverflow) {
					t.Fatalf("error=%v want bounded context failure", err)
				}
				if f.adopted != 0 || len(f.requests) != 2 || f.executed != 2 || f.committed != 2 {
					t.Fatalf("adopted/requests/executed/committed=%d/%d/%d/%d", f.adopted, len(f.requests), f.executed, f.committed)
				}
				last := f.requests[len(f.requests)-1]
				if cfg.ContextMutations.FinalInputHash() != contextfrag.ProviderPayloadHash(last.System, last.Messages, last.Tools) {
					t.Fatal("failed candidate changed dispatched hash")
				}
				fork, err := cfg.ForkContext.Messages()
				if err != nil || !reflect.DeepEqual(fork, f.forks[len(f.forks)-1]) {
					t.Fatal("failed candidate changed fork")
				}
			})
		}
	}
}

func TestContextSwitchCanceledCandidateIsNeverAdopted(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			a, cfg, f := newContextSwitchFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			original := cfg.SwitchContext
			cfg.SwitchContext = func(ctx context.Context, input ContextSwitchInput) (ContextSwitchResult, error) {
				result, err := original(ctx, input)
				cancel()
				return result, err
			}
			_ = runContextSwitchFixture(ctx, a, cfg, stream)
			if len(f.requests) != 2 || f.adopted != 0 || f.committed != 2 {
				t.Fatalf("requests/adopted/committed=%d/%d/%d", len(f.requests), f.adopted, f.committed)
			}
			last := f.requests[1]
			if cfg.ContextMutations.FinalInputHash() != contextfrag.ProviderPayloadHash(last.System, last.Messages, last.Tools) {
				t.Fatal("canceled candidate changed dispatched hash")
			}
		})
	}
}
