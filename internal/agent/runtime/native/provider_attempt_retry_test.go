package native

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
)

func round8RetryToolCycles(cycles, resultBytes int) []sdk.Message {
	messages := make([]sdk.Message, 0, cycles*2)
	for i := 0; i < cycles; i++ {
		callID := fmt.Sprintf("round8-retry-%02d", i)
		messages = append(messages,
			sdk.Message{
				Role: sdk.MessageRoleAssistant,
				Content: []sdk.MessagePart{sdk.ToolCallPart{
					ToolCallID: callID,
					ToolName:   "lookup",
					Input:      map[string]any{"step": i},
				}},
			},
			sdk.ToolMessage(sdk.ToolResultPart{
				ToolCallID: callID,
				ToolName:   "lookup",
				Result:     strings.Repeat("x", resultBytes),
			}),
		)
	}
	return messages
}

func pruneRound8RetryOldToolResults(messages []sdk.Message, keepRecent int) ([]sdk.Message, int) {
	out := append([]sdk.Message(nil), messages...)
	toolResults := 0
	for _, msg := range out {
		if msg.Role == sdk.MessageRoleTool {
			toolResults++
		}
	}
	toPrune := toolResults - keepRecent
	if toPrune < 1 {
		return out, 0
	}
	pruned := 0
	for i, msg := range out {
		if msg.Role != sdk.MessageRoleTool || pruned >= toPrune || len(msg.Content) == 0 {
			continue
		}
		result, ok := msg.Content[0].(sdk.ToolResultPart)
		if !ok {
			continue
		}
		result.Result = "[tool result pruned for round8 retry]"
		out[i] = sdk.ToolMessage(result)
		pruned++
	}
	return out, pruned
}

func countRound8PrunedToolResults(messages []sdk.Message) int {
	count := 0
	for _, msg := range messages {
		if msg.Role != sdk.MessageRoleTool || len(msg.Content) == 0 {
			continue
		}
		result, ok := msg.Content[0].(sdk.ToolResultPart)
		if !ok {
			continue
		}
		text, _ := result.Result.(string)
		if strings.Contains(text, "pruned for round8 retry") {
			count++
		}
	}
	return count
}

func runRound8MidStreamRetry(
	streamCtx context.Context,
	cancel context.CancelCauseFunc,
	a *Agent,
	cfg RunConfig,
	prevResult *sdk.StreamResult,
) (*sdk.StreamResult, bool) {
	return a.runMidStreamRetry(
		context.Background(),
		streamCtx,
		cancel,
		newToolAbortRegistry(),
		make(chan StreamEvent, 256),
		cfg,
		nil,
		nil,
		nil,
		prevResult,
		0,
		"api error 500",
		&strings.Builder{},
		nil,
	)
}

func TestRunMidStreamRetryWindowZeroAppliesAccumulatedSuffixHygiene(t *testing.T) {
	t.Parallel()

	ledger := contextfrag.NewMutationLedger()
	ledger.AppendStepSnapshot(contextfrag.StepSnapshot{StepIndex: 0, PostPrepareInputHash: "initial-hash"})
	var selectorCalls atomic.Int32
	var retryParams sdk.GenerateParams
	modelProvider := &atomicMockProvider{
		handler: func(_ int, params sdk.GenerateParams) (*sdk.GenerateResult, error) {
			retryParams = cloneGenerateParams(params)
			return &sdk.GenerateResult{Text: "ok", FinishReason: sdk.FinishReasonStop}, nil
		},
	}
	cfg := captureProviderAttemptPrefix(RunConfig{
		Model:                  &sdk.Model{ID: "mock-model", Provider: modelProvider},
		Messages:               []sdk.Message{sdk.UserMessage("original task")},
		Identity:               SessionContext{BotID: "bot-1"},
		ContextMutations:       ledger,
		ContextBudgetMaxTokens: 0,
		ContextStepReselector: func(_ context.Context, input ContextStepSelectionInput) ContextStepSelectionResult {
			selectorCalls.Add(1)
			if input.InitialMessageCount != 1 {
				t.Fatalf("InitialMessageCount = %d, want original turn prefix 1", input.InitialMessageCount)
			}
			if input.BudgetMaxTokens != 0 {
				t.Fatalf("BudgetMaxTokens = %d, want 0", input.BudgetMaxTokens)
			}
			if input.KeepRecentToolResults != stepReselectKeepRecentToolResults ||
				input.MinMessages != stepReselectMinMessages {
				t.Fatalf("hygiene settings = keep %d/min %d", input.KeepRecentToolResults, input.MinMessages)
			}
			selected, pruned := pruneRound8RetryOldToolResults(input.Messages, input.KeepRecentToolResults)
			return ContextStepSelectionResult{Messages: selected, Truncated: pruned}
		},
	})
	streamCtx, cancel := context.WithCancelCause(context.Background())
	installContextStepFailureHandler(&cfg, cancel)

	result, aborted := runRound8MidStreamRetry(
		streamCtx,
		cancel,
		New(Deps{}),
		cfg,
		&sdk.StreamResult{Messages: round8RetryToolCycles(10, 2048)},
	)
	if aborted {
		t.Fatal("runMidStreamRetry() aborted, want successful retry")
	}
	if result == nil {
		t.Fatal("runMidStreamRetry() result is nil")
	}
	if selectorCalls.Load() != 1 || modelProvider.calls.Load() != 1 {
		t.Fatalf("selector/provider calls = %d/%d, want 1/1", selectorCalls.Load(), modelProvider.calls.Load())
	}
	if got := countRound8PrunedToolResults(retryParams.Messages); got != 6 {
		t.Fatalf("pruned retry tool results = %d, want 6 old results with newest four intact", got)
	}
	if textOfMessage(retryParams.Messages[0]) != "original task" {
		t.Fatalf("retry prefix = %#v, want original task", retryParams.Messages[0])
	}

	steps := ledger.StepSnapshots()
	if len(steps) != 2 {
		t.Fatalf("step snapshots = %#v, want initial plus retry", steps)
	}
	retryStep := steps[1]
	if retryStep.Attempt != 1 || retryStep.StepIndex != 0 ||
		retryStep.ReselectionOutcome != contextfrag.ReselectionOutcomeApplied ||
		retryStep.Truncated != 6 {
		t.Fatalf("retry step = %#v", retryStep)
	}
	wantHash, _ := contextfrag.ProviderPayloadHashAndBytes(retryParams.System, retryParams.Messages, retryParams.Tools)
	if retryStep.PostPrepareInputHash != wantHash || ledger.FinalInputHash() != wantHash {
		t.Fatalf("retry/final hashes = %q/%q, want %q", retryStep.PostPrepareInputHash, ledger.FinalInputHash(), wantHash)
	}
	if countProviderAttemptMutations(ledger.Records(), contextfrag.MutationContextBudgetFailure) != 0 {
		t.Fatalf("window-zero retry recorded budget failure: %#v", ledger.Records())
	}
}

func TestRunMidStreamRetryProtectedHookOverflowFencesProvider(t *testing.T) {
	t.Parallel()

	ledger := contextfrag.NewMutationLedger()
	ledger.AppendStepSnapshot(contextfrag.StepSnapshot{StepIndex: 0, PostPrepareInputHash: "initial-hash"})
	marker := "round8-retry-protected-hook"
	var selectorCalls atomic.Int32
	modelProvider := &atomicMockProvider{
		handler: func(_ int, _ sdk.GenerateParams) (*sdk.GenerateResult, error) {
			return &sdk.GenerateResult{Text: "unexpected", FinishReason: sdk.FinishReasonStop}, nil
		},
	}
	plan := contextfrag.ContextBudgetPlan{Window: 128, OutputReserve: 64}
	cfg := captureProviderAttemptPrefix(RunConfig{
		Model:            &sdk.Model{ID: "mock-model", Provider: modelProvider},
		Messages:         []sdk.Message{sdk.UserMessage("original task")},
		Identity:         SessionContext{BotID: "bot-1"},
		ContextMutations: ledger,
		ContextManifest:  contextfrag.Manifest{BudgetPlan: &plan},
		ContextStepReselector: func(_ context.Context, input ContextStepSelectionInput) ContextStepSelectionResult {
			selectorCalls.Add(1)
			if input.InitialMessageCount != 1 {
				t.Fatalf("InitialMessageCount = %d, want 1", input.InitialMessageCount)
			}
			if !providerAttemptContainsText(input.Messages[input.InitialMessageCount:], marker) {
				t.Fatalf("protected hook is not in retry suffix: %#v", input.Messages)
			}
			return ContextStepSelectionResult{FatalError: contextfrag.ErrProtectedContextOverflow}
		},
	})
	cfg = applyBeforeModelCallAppendContext(cfg, marker+"\n"+strings.Repeat("protected ", 1000))
	streamCtx, cancel := context.WithCancelCause(context.Background())
	installContextStepFailureHandler(&cfg, cancel)

	_, aborted := runRound8MidStreamRetry(
		streamCtx,
		cancel,
		New(Deps{}),
		cfg,
		&sdk.StreamResult{},
	)
	if !aborted {
		t.Fatal("runMidStreamRetry() did not abort on protected overflow")
	}
	if !errors.Is(context.Cause(streamCtx), contextfrag.ErrProtectedContextOverflow) {
		t.Fatalf("stream cause = %v, want protected overflow", context.Cause(streamCtx))
	}
	if selectorCalls.Load() != 1 {
		t.Fatalf("selector calls = %d, want 1", selectorCalls.Load())
	}
	if modelProvider.calls.Load() != 0 {
		t.Fatalf("provider calls = %d, want 0", modelProvider.calls.Load())
	}

	steps := ledger.StepSnapshots()
	if len(steps) != 2 {
		t.Fatalf("step snapshots = %#v, want initial plus failed retry", steps)
	}
	failed := steps[1]
	if failed.Attempt != 1 || failed.StepIndex != 0 ||
		failed.ReselectionOutcome != contextfrag.ReselectionOutcomeFailed ||
		failed.PostPrepareInputHash != "" {
		t.Fatalf("failed retry snapshot = %#v", failed)
	}
	if got := countProviderAttemptMutations(ledger.Records(), contextfrag.MutationContextBudgetFailure); got != 1 {
		t.Fatalf("budget failure mutations = %d, want 1", got)
	}
	if got := countProviderAttemptMutations(ledger.Records(), contextfrag.MutationMidStreamRetry); got != 1 {
		t.Fatalf("mid-stream retry mutations = %d, want 1", got)
	}
}
