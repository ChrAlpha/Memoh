package application

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/chat/timeline"
	"github.com/felinics/memoh/internal/contextview"
)

func discussBudgetRecoveryFixture(t *testing.T) (*Service, *fakeDiscussService, *countingDiscussLifecycleProvider, *gatedCompactionRunner, turn.StartTurnCommand) {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	provider := &countingDiscussLifecycleProvider{}
	resolver := &fakeDiscussService{resolveResult: ResolveRunConfigResult{
		RunConfig: native.RunConfig{
			Model:           &sdk.Model{ID: "discuss-budget", Provider: provider},
			System:          strings.Repeat("s", 3200),
			ContextToolDefs: []contextfrag.ToolDefAccounting{{Name: "large_roster", TokenEstimate: 10000}},
		},
		ContextBudgetMaxTokens: 16000,
	}}
	agent := native.New(native.Deps{Logger: logger, ContextViewApplier: contextview.ProviderRunConfigApplier(logger)})
	service := newDiscussTestService(&fakeRunner{}, agent, resolver)
	policyService, _ := newControllerPolicyService(t, nil)
	service.settingsService = policyService.settingsService
	service.modelsService = policyService.modelsService
	service.queries = policyService.queries
	runner := newGatedCompactionRunner()
	t.Cleanup(func() { close(runner.release) })
	service.compactionService = runner
	cmd := lifecycleDiscussCommand()
	cmd.DiscussMessages = nil
	for i := 0; i < 20; i++ {
		cmd.DiscussMessages = append(cmd.DiscussMessages, turn.DiscussMessage{
			Role: "user", Content: strings.Repeat("s", 200), CompactionArtifactID: fmt.Sprintf("summary-%d", i),
		})
	}
	cmd.DiscussMessages = append(cmd.DiscussMessages,
		turn.DiscussMessage{Role: "assistant", Content: strings.Repeat("h", 8000)},
		turn.DiscussMessage{Role: "user", Content: "continue"},
	)
	return service, resolver, provider, runner, cmd
}

func TestDiscussProviderAdmissionLeavesFittingHistoryUnchanged(t *testing.T) {
	service, resolver, _, runner, cmd := discussBudgetRecoveryFixture(t)
	configureDiscussLifecycle(service)
	provider := &triggerLifecycleProvider{}
	resolver.resolveResult.RunConfig.Model.Provider = provider
	history, current := strings.Repeat("h", 160), strings.Repeat("c", 1600)
	cmd.DiscussMessages = []turn.DiscussMessage{
		{Role: "assistant", Content: history},
		{Role: "user", Content: current},
	}
	handle, err := service.StartTurn(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	for range handle.Events() {
	}
	for err := range handle.Errs() {
		t.Errorf("fitting history turn failed: %v", err)
	}
	if provider.callCount() != 1 || resolver.storeCalls != 1 {
		t.Fatalf("provider/history=%d/%d, want 1/1 for 50 history tokens within the final 500-token allowance", provider.callCount(), resolver.storeCalls)
	}
	runner.assertNoPass(t)
	assertSDKMessagesEqual(t, provider.params.Messages, []sdk.Message{sdk.AssistantMessage(history), sdk.UserMessage(current)})
}

func TestDiscussProviderAdmissionNeverWaitsForCompaction(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		t.Run(fmt.Sprintf("overflow=%t", overflow), func(t *testing.T) {
			service, resolver, provider, runner, cmd := discussBudgetRecoveryFixture(t)
			configureDiscussLifecycle(service)
			cmd.DiscussMessages = cmd.DiscussMessages[20:]
			if overflow {
				resolver.resolveResult.RunConfig.ContextSourceFrags = []contextfrag.ContextFrag{contextfrag.MessageFrag(contextfrag.MessageFragInput{
					ID: "hook", Message: sdk.UserMessage(strings.Repeat("hook", 800)),
					Kind: contextfrag.KindHookContext, Slot: contextfrag.SlotAfterHistoryBeforeCurrent,
					Budget: contextfrag.BudgetPolicy{Overflow: contextfrag.OverflowKeep},
				})}
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			handle, err := service.StartTurn(ctx, cmd)
			if err != nil {
				t.Fatal(err)
			}
			for range handle.Events() {
			}
			var failure error
			for err := range handle.Errs() {
				failure = err
			}
			if ctx.Err() != nil {
				t.Fatal("provider admission waited for compaction")
			}
			if overflow {
				if apperror.CodeOf(failure) != apperror.CodeContextProtectedOverflow || provider.callCount() != 0 {
					t.Fatalf("protected overflow: err=%v calls=%d", failure, provider.callCount())
				}
			} else if failure != nil || provider.callCount() != 1 {
				t.Fatalf("fitting current input did not proceed: err=%v calls=%d", failure, provider.callCount())
			}
			if overflow {
				runner.assertNoPass(t)
			} else {
				runner.awaitPass(t)
			}
		})
	}
}

func echoDiscussImages(_ context.Context, _ string, refs []timeline.ImageAttachmentRef) [][]sdk.ImagePart {
	out := make([][]sdk.ImagePart, len(refs))
	for i, ref := range refs {
		out[i] = []sdk.ImagePart{{Image: "data:image/png;base64," + ref.ContentHash, MediaType: "image/png"}}
	}
	return out
}

func TestDiscussImageAdmissionDoesNotRequireRecovery(t *testing.T) {
	service, resolver, provider, runner, cmd := discussBudgetRecoveryFixture(t)
	configureDiscussLifecycle(service)
	service.queries.(*controllerQueries).settings.CompactionEnabled = false
	resolver.resolveResult.RunConfig.SupportsImageInput = true
	service.turnHooks.inlineImages = echoDiscussImages
	cmd.DiscussMessages = []turn.DiscussMessage{
		{Role: "user", Content: "older-a " + strings.Repeat("a", 200), Source: &turn.ContextMessageSource{Kind: "external", ID: "older-a", Current: true}},
		{Role: "user", Content: "newest continue", Source: &turn.ContextMessageSource{Kind: "external", ID: "newest", Current: true}},
	}
	cmd.DiscussImageRefs = []turn.DiscussImageRef{{ContentHash: "aGFzaA==", Mime: "image/png", MessageID: "older-a"}}
	h, err := service.StartTurn(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	drainDiscuss(t, h)
	if provider.callCount() != 1 || countRecoveryText(provider.params.Messages, "older-a ") != 1 {
		t.Fatalf("local admission lost text: provider=%d messages=%+v", provider.callCount(), provider.params.Messages)
	}
	runner.assertNoPass(t)
}
