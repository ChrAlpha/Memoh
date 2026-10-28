package native

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/jsonschema-go/jsonschema"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/step"
	agenttools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
)

func TestAgentGenerateCompactsProviderOverflowBeforeRetryingSameStep(t *testing.T) {
	history := sdk.UserMessage(strings.Repeat("old evidence ", 8000))
	input := sdk.UserMessage("Preserve the current task")
	folds, commits := 0, 0
	provider := &atomicMockProvider{handler: func(call int, req sdk.Request) (sdk.ModelResult, error) {
		if call == 1 {
			return sdk.ModelResult{}, &sdk.APIError{StatusCode: 400, Code: "context_length_exceeded"}
		}
		if folds != 1 || len(req.Messages) != 2 || !providerAttemptContainsText(req.Messages, "Preserve the current task") || providerAttemptContainsText(req.Messages, "old evidence") {
			t.Fatalf("retry did not adopt the committed fold: folds=%d messages=%d", folds, len(req.Messages))
		}
		return sdk.ModelResult{Text: "recovered", FinishReason: sdk.FinishReasonStop}, nil
	}}
	cfg := generateRetryRun(provider, fastRetry)
	cfg.Messages = []sdk.Message{history, input}
	cfg.ContextBudgetMaxTokens, cfg.ModelMaxOutputTokens = 200000, 8000
	cfg.ContextProvenance = []MessageProvenance{{Message: history, Sources: rowSources("history")}, {Message: input, Sources: rowSources("input"), Anchor: true}}
	cfg.SwitchContext = func(_ context.Context, sw ContextSwitch) (string, error) {
		folds++
		if sw.UsageTokens >= 192000 {
			t.Fatal("fixture must pass the original local budget")
		}
		sw.Fold(ContextFold{SummaryID: "checkpoint", Summary: sdk.UserMessage("Earlier evidence summarized"), Order: []int{-1, 1}})
		return "committed checkpoint", nil
	}
	cfg.OnStepCommitted = func(_ context.Context, index int, _ *step.Record) (StepDirective, error) {
		if index != 0 {
			t.Fatalf("step index=%d, want original step 0", index)
		}
		commits++
		return StepDirective{}, nil
	}
	result, err := New(Deps{}).Generate(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "recovered" || folds != 1 || commits != 1 || provider.calls.Load() != 2 {
		t.Fatalf("text=%q folds=%d commits=%d calls=%d", result.Text, folds, commits, provider.calls.Load())
	}
	if estimate := contextfrag.ProviderEnvelopeTokens("", cfg.Messages, nil); estimate >= 192000 {
		t.Fatal("fixture exceeds configured allowance")
	}
}

func TestEmptyContextExhaustionRecoversBeforeCommittingInBothLoops(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			folds, commits := 0, 0
			provider := &atomicMockProvider{handler: func(call int, req sdk.Request) (sdk.ModelResult, error) {
				if call == 1 {
					return sdk.ModelResult{FinishReason: sdk.FinishReasonLength, Usage: sdk.Usage{InputTokens: 1048570, TotalTokens: 1048570}}, nil
				}
				if folds != 1 || providerAttemptContainsText(req.Messages, "old evidence") || !providerAttemptContainsText(req.Messages, "current task") {
					t.Fatal("empty exhausted response retried without a committed reduction")
				}
				return sdk.ModelResult{Text: "done", FinishReason: sdk.FinishReasonStop}, nil
			}}
			history := sdk.UserMessage(strings.Repeat("old evidence ", 8000))
			input := sdk.UserMessage("current task")
			cfg := generateRetryRun(provider, fastRetry)
			cfg.Messages = []sdk.Message{history, input}
			cfg.ContextBudgetMaxTokens, cfg.ModelMaxOutputTokens = 1000000, 8192
			cfg.ContextProvenance = []MessageProvenance{{Message: history, Sources: rowSources("history")}, {Message: input, Sources: rowSources("input"), Anchor: true}}
			cfg.SwitchContext = func(_ context.Context, sw ContextSwitch) (string, error) {
				folds++
				sw.Fold(ContextFold{SummaryID: "checkpoint", Summary: sdk.UserMessage("Earlier evidence summarized"), Order: []int{-1, 1}})
				return "committed checkpoint", nil
			}
			cfg.OnStepCommitted = func(_ context.Context, index int, record *step.Record) (StepDirective, error) {
				commits++
				if index != 0 || record.Result.Text != "done" {
					t.Errorf("committed empty exhaustion as completed work: index=%d text=%q", index, record.Result.Text)
				}
				return StepDirective{}, nil
			}
			a := New(Deps{})
			if stream {
				for event := range a.Stream(context.Background(), cfg) {
					if event.Type == EventError {
						t.Fatal(event.Cause)
					}
					if event.Type == EventRuntimeNotice {
						t.Error("recovered empty attempt emitted an output-truncation notice")
					}
				}
			} else if _, err := a.Generate(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			if folds != 1 || commits != 1 || provider.calls.Load() != 2 {
				t.Fatalf("folds=%d commits=%d calls=%d", folds, commits, provider.calls.Load())
			}
		})
	}
}

func TestContextOverflowKeepsCommittedToolEffectsInBothLoops(t *testing.T) {
	for _, emptyLength := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("empty_length=%v/stream=%v", emptyLength, stream), func(t *testing.T) {
				effects, commits, folds, committedInputs := 0, 0, 0, 0
				provider := &atomicMockProvider{handler: func(call int, req sdk.Request) (sdk.ModelResult, error) {
					switch call {
					case 1:
						return sdk.ModelResult{FinishReason: sdk.FinishReasonToolCalls, ToolCalls: []sdk.ToolCall{{ToolCallID: "effect-1", ToolName: "effect", Input: toolexec.ArgumentsFromValue(map[string]any{})}}}, nil
					case 2:
						if emptyLength {
							return sdk.ModelResult{FinishReason: sdk.FinishReasonLength, Usage: sdk.Usage{InputTokens: 1048570, TotalTokens: 1048570}}, nil
						}
						return sdk.ModelResult{}, &sdk.APIError{Code: "context_length_exceeded"}
					default:
						if !providerAttemptContainsText(req.Messages, "STEER: retain the current goal") {
							t.Fatal("retry lost the admitted steer")
						}
						if effects != 1 || folds != 1 || commits != 1 {
							t.Fatalf("effects=%d folds=%d commits=%d", effects, folds, commits)
						}
						if callID, resultID := unpairedToolParts(req.Messages); callID != "" || resultID != "" {
							t.Fatal("retry broke call/result pairing")
						}
						foundEffect := false
						for _, message := range req.Messages {
							for _, part := range message.Content {
								if result, ok := part.(sdk.ToolResultPart); ok && strings.Contains(result.Result.String(), "effect completed") {
									foundEffect = true
								}
							}
						}
						if !foundEffect {
							t.Fatal("retry lost the completed effect")
						}
						return sdk.ModelResult{Text: "done", FinishReason: sdk.FinishReasonStop}, nil
					}
				}}
				a := New(Deps{})
				a.SetToolProviders([]agenttools.ToolProvider{staticToolProvider{tools: []toolexec.Tool{{Name: "effect", Parameters: &jsonschema.Schema{Type: "object"}, Execute: func(*toolexec.ToolExecContext, sdk.ToolArguments) (sdk.ToolOutput, error) {
					effects++
					return sdk.TextOutput("effect completed"), nil
				}}}}})
				history := sdk.UserMessage(strings.Repeat("evidence ", 10000))
				input := sdk.UserMessage("Perform one effect then finish")
				cfg := generateRetryRun(provider, fastRetry)
				cfg.Messages = []sdk.Message{history, input}
				cfg.SupportsToolCall = true
				cfg.ContextBudgetMaxTokens, cfg.ModelMaxOutputTokens = 1000000, 8192
				cfg.ContextProvenance = []MessageProvenance{{Message: history, Sources: rowSources("history")}, {Message: input, Sources: rowSources("input"), Anchor: true}}
				cfg.SwitchContext = func(_ context.Context, sw ContextSwitch) (string, error) {
					folds++
					sw.Fold(foldCovering(sw, "checkpoint", sdk.UserMessage("Evidence summarized"), rowSources("history")...))
					return "committed", nil
				}
				cfg.OnStepCommitted = func(_ context.Context, index int, record *step.Record) (StepDirective, error) {
					if index != commits {
						t.Fatalf("commit index=%d, want %d", index, commits)
					}
					commits++
					if providerAttemptContainsText(record.Messages, "STEER: retain the current goal") {
						committedInputs++
					}
					var provenance []MessageProvenance
					for i := range record.Messages {
						provenance = append(provenance, MessageProvenance{Sources: rowSources(fmt.Sprintf("step-%d-%d", index, i))})
					}
					dir := StepDirective{Provenance: provenance}
					if index == 0 {
						dir.NextInputs = []DirectiveInput{{ID: "steer-1", Text: "STEER: retain the current goal"}}
					}
					return dir, nil
				}
				if stream {
					for event := range a.Stream(context.Background(), cfg) {
						if event.Type == EventError {
							t.Fatal(event.Cause)
						}
					}
				} else if _, err := a.Generate(context.Background(), cfg); err != nil {
					t.Fatal(err)
				}
				if effects != 1 || commits != 2 || folds != 1 || provider.calls.Load() != 3 {
					t.Fatalf("effects=%d commits=%d folds=%d calls=%d", effects, commits, folds, provider.calls.Load())
				}
				if committedInputs != 1 {
					t.Fatalf("persisted steers=%d, want the admitted steer exactly once", committedInputs)
				}
			})
		}
	}
}

func TestLengthFinishesDoNotReplayCompletedOrUnmeasuredWork(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result sdk.ModelResult
	}{
		{"text", sdk.ModelResult{Text: "partial answer", FinishReason: sdk.FinishReasonLength, Usage: sdk.Usage{InputTokens: 1048570}}},
		{"output budget", sdk.ModelResult{FinishReason: sdk.FinishReasonLength, Usage: sdk.Usage{InputTokens: 1048570, OutputTokens: 8192}}},
		{"input fits", sdk.ModelResult{FinishReason: sdk.FinishReasonLength, Usage: sdk.Usage{InputTokens: 991808}}},
		{"missing usage", sdk.ModelResult{FinishReason: sdk.FinishReasonLength}},
		{"intentional silence", sdk.ModelResult{FinishReason: sdk.FinishReasonStop, Usage: sdk.Usage{InputTokens: 1048570}}},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				provider := &atomicMockProvider{handler: func(int, sdk.Request) (sdk.ModelResult, error) { return tc.result, nil }}
				cfg := generateRetryRun(provider, fastRetry)
				cfg.ContextBudgetMaxTokens, cfg.ModelMaxOutputTokens = 1000000, 8192
				cfg.SwitchContext = func(context.Context, ContextSwitch) (string, error) {
					t.Fatal("finished output was compacted for replay")
					return "", nil
				}
				a := New(Deps{})
				if stream {
					for event := range a.Stream(context.Background(), cfg) {
						if event.Type == EventError {
							t.Fatal(event.Cause)
						}
					}
				} else if _, err := a.Generate(context.Background(), cfg); err != nil {
					t.Fatal(err)
				}
				if provider.calls.Load() != 1 {
					t.Fatalf("calls=%d, want no replay", provider.calls.Load())
				}
			})
		}
	}
}

func TestStreamLengthFinishDoesNotReplayNonTextOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		part sdk.StreamPart
	}{
		{"file", &sdk.StreamFilePart{File: sdk.GeneratedFile{MediaType: "image/png", Data: "eA=="}}},
		{"source", &sdk.StreamSourcePart{}},
		{"reasoning", &sdk.ReasoningDeltaPart{ID: "reasoning", Text: "already reasoned"}},
		{"tool input", &sdk.ToolInputStartPart{ID: "partial", ToolName: "effect"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			provider := agentStreamTestProvider(func(context.Context, sdk.Request) (<-chan sdk.StreamPart, error) {
				calls++
				return closedAgentTestStream(tc.part, &sdk.FinishStepPart{FinishReason: sdk.FinishReasonLength, Usage: sdk.Usage{InputTokens: 1048570}}), nil
			})
			cfg := generateRetryRun(provider, fastRetry)
			cfg.ContextBudgetMaxTokens, cfg.ModelMaxOutputTokens = 1000000, 8192
			cfg.SwitchContext = func(context.Context, ContextSwitch) (string, error) {
				t.Fatal("non-text output was compacted for replay")
				return "", nil
			}
			for event := range New(Deps{}).Stream(context.Background(), cfg) {
				if event.Type == EventError {
					t.Fatal(event.Cause)
				}
			}
			if calls != 1 {
				t.Fatalf("calls=%d, want no replay", calls)
			}
		})
	}
}

func TestStreamContextOverflowOnlyRecoversBeforeOutput(t *testing.T) {
	for _, output := range []string{"none", "before error", "after error"} {
		t.Run(output, func(t *testing.T) {
			calls, folds := 0, 0
			provider := agentStreamTestProvider(func(_ context.Context, _ sdk.Request) (<-chan sdk.StreamPart, error) {
				calls++
				if calls == 1 {
					parts := []sdk.StreamPart{&sdk.StartStepPart{}}
					if output == "before error" {
						parts = append(parts, &sdk.TextDeltaPart{ID: "text", Text: "partial answer"})
					}
					parts = append(parts, &sdk.ErrorPart{Error: &sdk.APIError{Code: "context_length_exceeded"}})
					if output == "after error" {
						parts = append(parts, &sdk.TextDeltaPart{ID: "text", Text: "buffered answer"})
					}
					return closedAgentTestStream(parts...), nil
				}
				return closedAgentTestStream(&sdk.TextDeltaPart{ID: "text", Text: "done"}, &sdk.FinishStepPart{FinishReason: sdk.FinishReasonStop}), nil
			})
			cfg := generateRetryRun(provider, fastRetry)
			history := sdk.UserMessage(strings.Repeat("history ", 10000))
			cfg.Messages = []sdk.Message{history, sdk.UserMessage("task")}
			cfg.ContextBudgetMaxTokens, cfg.ModelMaxOutputTokens = 200000, 8000
			cfg.ContextProvenance = []MessageProvenance{{Message: history, Sources: rowSources("history")}}
			cfg.SwitchContext = func(_ context.Context, sw ContextSwitch) (string, error) {
				folds++
				sw.Fold(ContextFold{SummaryID: "checkpoint", Summary: sdk.UserMessage("summary"), Order: []int{-1, 1}})
				return "committed", nil
			}
			failed := false
			for event := range New(Deps{}).Stream(context.Background(), cfg) {
				failed = failed || event.Type == EventError
			}
			if output != "none" {
				if !failed || calls != 1 || folds != 0 {
					t.Fatalf("failed=%v calls=%d folds=%d", failed, calls, folds)
				}
			} else if failed || calls != 2 || folds != 1 {
				t.Fatalf("failed=%v calls=%d folds=%d", failed, calls, folds)
			}
		})
	}
}

func TestContextOverflowDoesNotRetryWithoutCommittedReduction(t *testing.T) {
	for _, mode := range []string{"no pass", "unchanged", "commit failure", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			provider := &atomicMockProvider{handler: func(int, sdk.Request) (sdk.ModelResult, error) {
				return sdk.ModelResult{}, &sdk.APIError{Code: "context_length_exceeded"}
			}}
			cfg := generateRetryRun(provider, fastRetry)
			cfg.ContextBudgetMaxTokens = 200000
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cfg.SwitchContext = func(_ context.Context, sw ContextSwitch) (string, error) {
				switch mode {
				case "unchanged":
					sw.Fold(ContextFold{Order: []int{0}})
				case "commit failure":
					return "", errors.New("checkpoint commit failed")
				case "cancel":
					cancel()
					return "", ctx.Err()
				}
				return "", nil
			}
			_, err := New(Deps{}).Generate(ctx, cfg)
			if err == nil || provider.calls.Load() != 1 {
				t.Fatalf("err=%v calls=%d", err, provider.calls.Load())
			}
		})
	}
}

func TestAgentUsesSuccessfulUsageBeforeNextTurnWithoutReplayingSuccess(t *testing.T) {
	history := sdk.UserMessage(strings.Repeat("dense evidence ", 8000))
	input := sdk.UserMessage("first task")
	folds := 0
	provider := &atomicMockProvider{handler: func(call int, _ sdk.Request) (sdk.ModelResult, error) {
		if call == 2 && folds != 1 {
			t.Error("next request ignored successful over-budget usage")
		}
		return sdk.ModelResult{Text: "done", FinishReason: sdk.FinishReasonStop, Usage: sdk.Usage{InputTokens: 218601, CachedInputTokens: 200000, OutputTokens: 15, TotalTokens: 218616}}, nil
	}}
	cfg := generateRetryRun(provider, fastRetry)
	cfg.Identity.SessionID = "usage-session"
	cfg.Messages = []sdk.Message{history, input}
	cfg.ContextBudgetMaxTokens, cfg.ModelMaxOutputTokens = 200000, 8000
	cfg.ContextProvenance = []MessageProvenance{{Message: history, Sources: rowSources("history")}, {Message: input, Sources: rowSources("input"), Anchor: true}}
	cfg.SwitchContext = func(_ context.Context, sw ContextSwitch) (string, error) {
		folds++
		if sw.UsageTokens < 218601 || sw.UsageTokens > 250000 {
			t.Fatalf("input usage=%d: cache/output tokens must not be counted again", sw.UsageTokens)
		}
		order := []int{-1}
		for i := 1; i < len(sw.Request.Messages); i++ {
			order = append(order, i)
		}
		sw.Fold(ContextFold{SummaryID: "checkpoint", Summary: sdk.UserMessage("Evidence summarized"), Order: order})
		return "committed", nil
	}
	a := New(Deps{})
	if _, err := a.Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if folds != 0 {
		t.Fatal("successful response was replayed or compacted before next request")
	}
	cfg.Messages = append(cfg.Messages, sdk.AssistantMessage("done"), sdk.UserMessage("follow up"))
	if _, err := a.Generate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if folds != 1 || provider.calls.Load() != 2 {
		t.Fatalf("folds=%d calls=%d", folds, provider.calls.Load())
	}
}

func TestUsageBaselineIsInvalidatedByRequestChangesOrMissingUsage(t *testing.T) {
	for _, change := range []string{"system", "model", "endpoint", "history", "unicode JSON", "image", "tool schema", "zero usage"} {
		t.Run(change, func(t *testing.T) {
			provider := &atomicMockProvider{handler: func(int, sdk.Request) (sdk.ModelResult, error) {
				usage := sdk.Usage{InputTokens: 218601}
				if change == "zero usage" {
					usage = sdk.Usage{}
				}
				return sdk.ModelResult{Text: "done", FinishReason: sdk.FinishReasonStop, Usage: usage}, nil
			}}
			cfg := generateRetryRun(provider, fastRetry)
			cfg.Identity.SessionID = "usage-scope"
			cfg.CurrentModelUUID = "catalog-model"
			cfg.ModelEndpoint = "https://endpoint-one.test"
			cfg.Messages = []sdk.Message{sdk.UserMessage(strings.Repeat("evidence ", 10000)), sdk.UserMessage("task")}
			cfg.ContextBudgetMaxTokens, cfg.ModelMaxOutputTokens = 200000, 8000
			cfg.SupportsImageInput = change == "image"
			cfg.SupportsToolCall = change == "tool schema"
			cfg.SwitchContext = func(context.Context, ContextSwitch) (string, error) {
				t.Error("reused usage from another request")
				return "", errors.New("unexpected fold")
			}
			a := New(Deps{})
			setSchema := func(value string) {
				a.SetToolProviders([]agenttools.ToolProvider{staticToolProvider{tools: []toolexec.Tool{{
					Name: "lookup", Parameters: &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{"mode": {Type: "string", Enum: []any{value}}}},
					Execute: func(*toolexec.ToolExecContext, sdk.ToolArguments) (sdk.ToolOutput, error) {
						return sdk.TextOutput("unused"), nil
					},
				}}}})
			}
			if change == "tool schema" {
				setSchema("first")
			}
			if _, err := a.Generate(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "system":
				cfg.System = "A different system instruction"
			case "model":
				model := *cfg.Model
				model.ID = "other-model"
				cfg.Model = &model
			case "endpoint":
				cfg.ModelEndpoint = "https://endpoint-two.test"
			case "history":
				cfg.Messages[0] = sdk.UserMessage("Edited history")
			case "unicode JSON":
				cfg.Messages[0] = sdk.UserMessage(strings.Repeat(`{"任务":"诊断","数据":[123456789,987654321]}`, 2000))
			case "image":
				cfg.Messages[0].Content = append(cfg.Messages[0].Content, sdk.ImagePart{Image: "data:image/png;base64,AAAA", MediaType: "image/png"})
			case "tool schema":
				setSchema("second")
			}
			cfg.Messages = append(cfg.Messages, sdk.AssistantMessage("done"), sdk.UserMessage("next"))
			if _, err := a.Generate(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestContextOverflowRecoveryIsBoundedPerStep(t *testing.T) {
	provider := &atomicMockProvider{handler: func(int, sdk.Request) (sdk.ModelResult, error) {
		return sdk.ModelResult{}, &sdk.APIError{Code: "context_length_exceeded"}
	}}
	cfg := generateRetryRun(provider, fastRetry)
	cfg.ContextBudgetMaxTokens = 200000
	cfg.Messages = []sdk.Message{sdk.UserMessage(strings.Repeat("history ", 8000)), sdk.UserMessage("task")}
	folds := 0
	cfg.SwitchContext = func(_ context.Context, sw ContextSwitch) (string, error) {
		folds++
		text := sw.Request.Messages[0].Content[0].(sdk.TextPart).Text
		sw.Fold(ContextFold{SummaryID: fmt.Sprintf("summary-%d", folds), Summary: sdk.UserMessage(text[:len(text)/2]), Order: []int{-1, 1}})
		return "committed", nil
	}
	_, err := New(Deps{}).Generate(context.Background(), cfg)
	if !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) || folds != 2 || provider.calls.Load() != 3 {
		t.Fatalf("err=%v folds=%d calls=%d", err, folds, provider.calls.Load())
	}
}
