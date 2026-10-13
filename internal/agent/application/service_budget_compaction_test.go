package application

import (
	"reflect"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	agenttools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/contextview"
)

func TestFinalHistoryBudgetSchedulesBelowAutomaticThreshold(t *testing.T) {
	s, history, cfg := chatBudgetFixture(t)
	runner := newGatedCompactionRunner()
	defer close(runner.release)
	s.compactionService = runner
	cfg.Messages = []sdk.Message{sdk.AssistantMessage(strings.Repeat("h", 8000)), sdk.UserMessage("continue")}
	cfg.ContextFrags = nil
	cfg.ContextTrimmableMessages = 1
	cfg.ContextCurrentUserMessageIndex = intPointer(1)
	cfg.ContextMemoryMessageIndex = nil
	cfg.ContextHookText = ""
	cfg.ContextSourceFrags = buildProviderSourceFrags(t.Context(), cfg, []native.SystemSection{{ID: "system", Kind: contextfrag.KindSystemPrompt, Text: strings.Repeat("s", 3200)}}, nil)
	cfg.ObserveContextBudget = s.chatBudgetObserver(ChatRequest{BotID: syncCompactBotID, ThreadID: syncCompactThreadID}, chatHistoryLayout{historyCount: 1, pressureTokens: 2500})
	ctx, endRun := WithRunEnd(t.Context())
	got, err := contextview.ProviderRunConfigApplier(nil)(ctx, cfg)
	if err != nil || history.loads != 0 || countRecoveryText(got.Messages, "continue") != 1 || countRecoveryText(got.Messages, "compacted history") != 0 {
		t.Fatalf("one-pass admission: error=%v loads=%d messages=%v", err, history.loads, got.Messages)
	}
	runner.assertNoPass(t)
	endRun()
	pass := runner.awaitPass(t)
	if pass.TargetTokens != 399 || pass.TotalInputTokens != 3125 {
		t.Fatalf("background demand lost final history allowance: target=%d pressure=%d", pass.TargetTokens, pass.TotalInputTokens)
	}
}

func TestChatAdmissionPreservesCurrentMediaAndForkSnapshot(t *testing.T) {
	s, history, cfg := chatBudgetFixture(t)
	cfg.ModelMaxOutputTokens = 2000
	cfg.ContextTrimmableMessages = 2
	cfg.Messages = []sdk.Message{sdk.AssistantMessage("frozen fork"), sdk.AssistantMessage(strings.Repeat("h", 8000)), sdk.UserMessage("frozen memory"), sdk.UserMessage("current request")}
	cfg.ContextCurrentUserMessageIndex = intPointer(3)
	cfg.ContextMemoryMessageIndex = intPointer(2)
	cfg.InlineImages = []sdk.ImagePart{{Image: "data:image/png;base64,YQ=="}}
	cfg.InlineAttachments = []sdk.MessagePart{sdk.FilePart{Data: "Yg==", MediaType: "application/pdf"}}
	cfg = s.prepareRunConfig(t.Context(), cfg)
	cfg.ForkContextSourceMessageIDs = []string{"inherited-source", "history-source", "memory-source", "current-source"}
	cfg.ForkContext = agenttools.NewMessageSnapshotWithSources(cfg.Messages, cfg.ForkContextSourceMessageIDs)
	before, err := cfg.ForkContext.Entries()
	if err != nil {
		t.Fatal(err)
	}
	current := cfg.Messages[3]
	cfg.ContextSourceFrags = buildProviderSourceFrags(t.Context(), cfg, []native.SystemSection{{ID: "system", Kind: contextfrag.KindSystemPrompt, Text: strings.Repeat("s", 3200)}}, nil)
	original := append([]contextfrag.ContextFrag(nil), cfg.ContextSourceFrags...)
	got, err := contextview.ProviderRunConfigApplier(nil)(t.Context(), cfg)
	if err != nil {
		t.Fatalf("%v plan=%+v manifest=%+v", err, got.ContextManifest.BudgetPlan, got.ContextManifest.Selection)
	}
	if got.ContextCurrentUserMessageIndex == nil || !reflect.DeepEqual(got.Messages[*got.ContextCurrentUserMessageIndex], current) || history.loads != 0 || !reflect.DeepEqual(original, cfg.ContextSourceFrags) {
		t.Fatal("one-pass admission changed current media, source graph or reloaded history")
	}
	after, err := got.ForkContext.Entries()
	if err != nil || got.ForkContext != cfg.ForkContext || !reflect.DeepEqual(before, after) {
		t.Fatal("admission changed the fork snapshot or its source identities")
	}
}

func TestBudgetObserverRetainsPressureAfterHistoryWasTrimmed(t *testing.T) {
	service, history, cfg := chatBudgetFixture(t)
	runner := newGatedCompactionRunner()
	defer close(runner.release)
	service.compactionService = runner
	cfg.ContextSourceFrags = buildProviderSourceFrags(t.Context(), cfg, []native.SystemSection{{ID: "system", Kind: contextfrag.KindSystemPrompt, Text: strings.Repeat("s", 3200)}}, nil)
	cfg.ObserveContextBudget = service.chatBudgetObserver(ChatRequest{BotID: syncCompactBotID, ThreadID: syncCompactThreadID}, chatHistoryLayout{pressureTokens: 20000})
	ctx, endRun := WithRunEnd(t.Context())
	got, err := contextview.ProviderRunConfigApplier(nil)(ctx, cfg)
	if err != nil || history.loads != 0 || countRecoveryText(got.Messages, "current request") != 1 {
		t.Fatalf("admission error=%v history loads=%d", err, history.loads)
	}
	runner.assertNoPass(t)
	endRun()
	if pass := runner.awaitPass(t); pass.TotalInputTokens != 25000 {
		t.Fatalf("lost pre-trim pressure: %+v", pass)
	}
}
