package native

import (
	"context"
	"fmt"
	"testing"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/jsonschema-go/jsonschema"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	agenttools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/models"
)

func TestBuildGenerateDispatchSetsModelInfo(t *testing.T) {
	t.Parallel()

	ledger := contextfrag.NewMutationLedger()
	model := &sdk.Model{ID: "mock-model", Provider: &usageRecordingProvider{}, Type: sdk.ModelTypeChat}
	cfg := RunConfig{
		Model:            model,
		System:           "sys",
		Messages:         []sdk.Message{sdk.UserMessage("hi")},
		ContextMutations: ledger,
	}

	if _, err := (*Agent)(nil).buildGenerateDispatch(context.Background(), cfg, nil, nil, nil); err != nil {
		t.Fatalf("buildGenerateDispatch: %v", err)
	}

	gotModel, gotClientType := ledger.ModelInfo()
	if gotModel != "mock-model" {
		t.Fatalf("model = %q, want mock-model", gotModel)
	}
	if gotClientType != string(models.ClientTypeOpenAICompletions) {
		t.Fatalf("client type = %q, want %q", gotClientType, models.ClientTypeOpenAICompletions)
	}
}

// anthropicNameMockProvider wraps atomicMockProvider so
// models.ResolveClientType resolves it to the Anthropic Messages client
// type (decoration dispatch keys off Provider.Name() containing
// "anthropic"), while DoGenerate/DoStream still run through the
// caller-supplied mock handler instead of a real network call.
type anthropicNameMockProvider struct {
	*atomicMockProvider
}

func (anthropicNameMockProvider) Name() string { return "anthropic-mock" }

// TestAgentGenerateRecordsOneStepSnapshotPerModelStepWithDistinctHashes
// covers Defect A: R provider calls must produce exactly R step snapshots
// (0..R-1), each hashing that call's ACTUAL input request — not R-1 snapshots
// misattributed one call ahead of the usage they are supposed to pair with.
// The owned generate loop builds the request for every step, including step 0,
// so each snapshot pairs with the call it was taken from.
func TestAgentGenerateRecordsOneStepSnapshotPerModelStepWithDistinctHashes(t *testing.T) {
	t.Parallel()

	var callParams []sdk.Request
	modelProvider := &atomicMockProvider{
		handler: func(call int, params sdk.Request) (sdk.ModelResult, error) {
			callParams = append(callParams, sdk.Request{
				System:   params.System,
				Messages: append([]sdk.Message(nil), params.Messages...),
				Tools:    append([]sdk.ToolDefinition(nil), params.Tools...),
			})
			if call <= 3 {
				return sdk.ModelResult{
					FinishReason: sdk.FinishReasonToolCalls,
					ToolCalls: []sdk.ToolCall{{
						ToolCallID: fmt.Sprintf("call-%d", call),
						ToolName:   "lookup",
						Input:      toolexec.ArgumentsFromValue(map[string]any{"step": call}),
					}},
				}, nil
			}
			return sdk.ModelResult{Text: "ok", FinishReason: sdk.FinishReasonStop}, nil
		},
	}

	a := New(Deps{})
	a.SetToolProviders([]agenttools.ToolProvider{
		staticToolProvider{tools: []toolexec.Tool{{
			Name:       "lookup",
			Parameters: &jsonschema.Schema{Type: "object"},
			Execute: func(_ *toolexec.ToolExecContext, _ sdk.ToolArguments) (sdk.ToolOutput, error) {
				return toolexec.OutputFromValue(map[string]any{"answer": "ok"}), nil
			},
		}}},
	})

	ledger := contextfrag.NewMutationLedger()
	_, err := a.Generate(context.Background(), RunConfig{
		Model:            &sdk.Model{ID: "mock-model", Provider: modelProvider},
		Messages:         []sdk.Message{sdk.UserMessage("start")},
		SupportsToolCall: true,
		Identity:         SessionContext{BotID: "bot-1"},
		ContextMutations: ledger,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	steps := ledger.StepSnapshots()
	if len(steps) != len(callParams) {
		t.Fatalf("step snapshots = %d, want %d (one per model step, matching provider calls)", len(steps), len(callParams))
	}
	seen := map[string]bool{}
	for i, step := range steps {
		if step.StepIndex != i {
			t.Fatalf("steps[%d].StepIndex = %d, want %d", i, step.StepIndex, i)
		}
		if step.PostPrepareInputHash == "" {
			t.Fatalf("steps[%d].PostPrepareInputHash empty", i)
		}
		wantHash := contextfrag.ProviderPayloadHash(callParams[i].System, callParams[i].Messages, callParams[i].Tools)
		if step.PostPrepareInputHash != wantHash {
			t.Fatalf("steps[%d].PostPrepareInputHash = %q, want hash of call %d's actual input %q", i, step.PostPrepareInputHash, i, wantHash)
		}
		if seen[step.PostPrepareInputHash] {
			t.Fatalf("steps[%d] hash %q duplicates an earlier step; messages differ across steps so hashes must too", i, step.PostPrepareInputHash)
		}
		seen[step.PostPrepareInputHash] = true
	}
	if got, want := ledger.FinalInputHash(), steps[len(steps)-1].PostPrepareInputHash; got != want {
		t.Fatalf("FinalInputHash() = %q, want last step hash %q", got, want)
	}
}

func TestPrepareMidStreamRetryConfigWithMessagesAdvancesAttemptForSubsequentRecords(t *testing.T) {
	t.Parallel()

	ledger := contextfrag.NewMutationLedger()
	ledger.RecordCacheUsage(contextfrag.CacheUsageRecord{StepIndex: 0})

	cfg := RunConfig{
		Messages:         []sdk.Message{sdk.UserMessage("hello")},
		ContextMutations: ledger,
	}
	_ = prepareMidStreamRetryConfigWithMessages(cfg, cfg.Messages, nil, 0, "timeout")
	ledger.RecordCacheUsage(contextfrag.CacheUsageRecord{StepIndex: 0})
	ledger.AppendStepSnapshot(contextfrag.StepSnapshot{StepIndex: 0})

	records := ledger.CacheUsageRecords()
	if len(records) != 2 || records[0].Attempt != 0 || records[1].Attempt != 1 {
		t.Fatalf("cache usage attempts = %#v, want [0, 1]", records)
	}
	steps := ledger.StepSnapshots()
	if len(steps) != 1 || steps[0].Attempt != 1 {
		t.Fatalf("step snapshot attempt = %#v, want 1", steps)
	}
}
