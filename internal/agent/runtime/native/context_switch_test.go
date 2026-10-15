package native

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/jsonschema-go/jsonschema"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/step"
	agenttools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
)

type contextSwitchFixture struct {
	toolSteps                    int
	forks                        [][]sdk.Message
	committed, executed, adopted int
	retry                        bool
	requests                     []sdk.Request
	inputs                       []ContextSwitchInput
}

func newContextSwitchFixture(t *testing.T) (*Agent, RunConfig, *contextSwitchFixture) {
	t.Helper()
	f := &contextSwitchFixture{toolSteps: 2}
	fork := agenttools.NewMessageSnapshot(nil)
	provider := &atomicMockProvider{handler: func(call int, req sdk.Request) (sdk.ModelResult, error) {
		f.requests = append(f.requests, cloneGenerateParams(req))
		snapshot, err := fork.Messages()
		if err != nil {
			t.Error(err)
		}
		f.forks = append(f.forks, snapshot)
		if call <= f.toolSteps {
			return sdk.ModelResult{FinishReason: sdk.FinishReasonToolCalls, ToolCalls: []sdk.ToolCall{{ToolCallID: fmt.Sprint("call-", call), ToolName: "lookup"}}}, nil
		}
		if f.retry && call == f.toolSteps+1 {
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
						result.Sources = append(result.Sources, ContextSwitchSource{Ref: contextfrag.ContextRef{Namespace: "compaction_summary", ID: "summary", Version: 7}})
					}
					if len(input.Sources[i].Coverage) > 0 {
						result.Sources[summaryIndex].Coverage = append(result.Sources[summaryIndex].Coverage, input.Sources[i].Coverage...)
					} else {
						result.Sources[summaryIndex].Coverage = append(result.Sources[summaryIndex].Coverage, input.Sources[i].Ref)
					}
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
				mutations := contextSwitchMutations(cfg)
				if len(mutations) != 1 || !strings.Contains(mutations[0].Detail, "summary_refs=compaction_summary:summary@v7") || !strings.Contains(mutations[0].Detail, "coverage_count=2") {
					t.Fatalf("adoption evidence lacks the summary and raw coverage: %+v", mutations)
				}
				if strings.Contains(mutations[0].Detail, "earlier work summary") || strings.Contains(mutations[0].Detail, "current request") {
					t.Fatalf("adoption evidence includes message content: %+v", mutations)
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
				if mutations := contextSwitchMutations(cfg); len(mutations) != 0 {
					t.Fatalf("rejected candidate recorded adoption: %+v", mutations)
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
			if mutations := contextSwitchMutations(cfg); len(mutations) != 0 {
				t.Fatalf("cancelled candidate recorded adoption: %+v", mutations)
			}
			last := f.requests[1]
			if cfg.ContextMutations.FinalInputHash() != contextfrag.ProviderPayloadHash(last.System, last.Messages, last.Tools) {
				t.Fatal("canceled candidate changed dispatched hash")
			}
		})
	}
}

func contextSwitchMutations(cfg RunConfig) []contextfrag.MutationRecord {
	var result []contextfrag.MutationRecord
	for _, record := range cfg.ContextMutations.Records() {
		if record.Kind == contextfrag.MutationContextSwitch {
			result = append(result, record)
		}
	}
	return result
}

func TestContextSwitchPreservesDuplicateDynamicInputIdentityAcrossRetry(t *testing.T) {
	a, cfg, f := newContextSwitchFixture(t)
	f.retry = true
	injected := make(chan InjectMessage, 1)
	injected <- InjectMessage{Text: "current request"}
	cfg.InjectCh = injected
	commit := cfg.OnStepCommitted
	cfg.OnStepCommitted = func(ctx context.Context, index int, record *step.Record) (StepDirective, error) {
		dir, err := commit(ctx, index, record)
		if index == 0 {
			dir.NextInputs = []DirectiveInput{{ID: "steer-item", Text: "current request"}}
		}
		return dir, err
	}
	var recorded []string
	cfg.InjectedRecorder = func(text string, _ int) { recorded = append(recorded, text) }
	if err := runContextSwitchFixture(context.Background(), a, cfg, true); err != nil {
		t.Fatal(err)
	}
	if f.adopted != 1 || len(f.requests) != 4 {
		t.Fatalf("adopted/requests=%d/%d", f.adopted, len(f.requests))
	}
	last := f.requests[len(f.requests)-1]
	if countRound8MessageText(last.Messages, "current request") != 3 {
		t.Fatal("switch merged equal-text user, injection, and steer inputs")
	}
	if !reflect.DeepEqual(recorded, []string{"current request", "current request"}) {
		t.Fatalf("recorded inputs=%v", recorded)
	}
	input := f.inputs[0]
	var sources []string
	for i, message := range input.Request.Messages {
		if countRound8MessageText([]sdk.Message{message}, "current request") == 1 {
			if !input.Protected[i] {
				t.Fatal("current input was not protected")
			}
			sources = append(sources, input.Sources[i].Ref.ID)
		}
	}
	if !reflect.DeepEqual(sources, []string{"", "step-1-message-0", "step-1-message-1"}) {
		t.Fatalf("duplicate-text sources=%v", sources)
	}
}

func TestContextSwitchCarriesSummaryCoverageAcrossSuccessiveSteps(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			a, cfg, f := newContextSwitchFixture(t)
			f.toolSteps = 6
			f.retry = true
			if err := runContextSwitchFixture(context.Background(), a, cfg, stream); err != nil {
				t.Fatal(err)
			}
			if f.committed != 7 || f.executed != 6 || len(f.requests) != 8 || f.adopted < 2 {
				t.Fatalf("committed/executed/requests/adopted=%d/%d/%d/%d", f.committed, f.executed, len(f.requests), f.adopted)
			}
			var summary ContextSwitchSource
			for _, source := range f.inputs[len(f.inputs)-1].Sources {
				if source.Ref.Namespace == "compaction_summary" {
					summary = source
				}
			}
			if len(summary.Coverage) != 8 {
				t.Fatalf("last summary's raw coverage=%d want first four steps' 8 messages", len(summary.Coverage))
			}
			for i, ref := range summary.Coverage {
				want := fmt.Sprintf("step-%d-message-%d", i/2, i%2)
				if ref.ID != want {
					t.Fatalf("coverage[%d]=%s want %s", i, ref.ID, want)
				}
			}
		})
	}
}

func TestContextSwitchSteerCheckpointKeepsLastCompleteInteraction(t *testing.T) {
	a, cfg, f := newContextSwitchFixture(t)
	cfg.ContextManifest.BudgetPlan.Window = 1900
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wake := make(chan struct{}, 1)
	var pending atomic.Bool
	cfg.SteerWake = wake
	cfg.PendingSteer = func(context.Context) (bool, error) { return pending.Load(), nil }
	cfg.OnSteer = func(ctx context.Context, index int, record *step.Record) (StepDirective, error) {
		dir, err := cfg.OnStepCommitted(ctx, index, record)
		pending.Store(false)
		dir.NextInputs = []DirectiveInput{{ID: "steer", Text: "change direction"}}
		return dir, err
	}
	original := cfg.Model.Provider
	var calls int
	partial := strings.Repeat("partial ", 200)
	cfg.Model.Provider = agentStreamTestProvider(func(ctx context.Context, request sdk.Request) (<-chan sdk.StreamPart, error) {
		calls++
		if calls <= 2 {
			return original.DoStream(ctx, request)
		}
		f.requests = append(f.requests, cloneGenerateParams(request))
		if calls == 3 {
			parts := make(chan sdk.StreamPart, 1)
			parts <- &sdk.TextDeltaPart{Text: partial}
			go func() { <-ctx.Done(); close(parts) }()
			return parts, nil
		}
		return closedAgentTestStream(&sdk.TextDeltaPart{Text: "done"}, &sdk.FinishStepPart{FinishReason: sdk.FinishReasonStop}), nil
	})
	for event := range a.Stream(ctx, cfg) {
		if event.Type == EventTextDelta && event.Delta == partial {
			pending.Store(true)
			wake <- struct{}{}
		}
		if event.Type == EventError || event.Type == EventAgentAbort {
			t.Fatalf("steer failed: %v", event.Cause)
		}
	}
	if calls != 4 || f.committed != 4 || f.executed != 2 || f.adopted != 1 {
		t.Fatalf("calls/committed/executed/adopted=%d/%d/%d/%d", calls, f.committed, f.executed, f.adopted)
	}
	if len(f.inputs) < 2 {
		t.Fatal("steer checkpoint did not reach switch pressure")
	}
	input := f.inputs[len(f.inputs)-1]
	if input.StepIndex != 3 {
		t.Fatalf("next durable step=%d want3", input.StepIndex)
	}
	var lastComplete int
	for i, source := range input.Sources {
		if strings.HasPrefix(source.Ref.ID, "step-1-") {
			lastComplete++
			if !input.Protected[i] {
				t.Fatal("interrupted checkpoint released previous complete interaction")
			}
		}
	}
	if lastComplete != 2 || !providerAttemptContainsText(f.requests[3].Messages, partial) || !providerAttemptContainsText(f.requests[3].Messages, "change direction") {
		t.Fatal("steer lost completed or current input")
	}
}
