package application

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/chat/timeline"
	"github.com/felinics/memoh/internal/contextview"
)

type discussBudgetCompactor struct {
	configs []compaction.TriggerConfig
	status  string
	err     error
	delay   time.Duration
	started chan struct{}
}

func (c *discussBudgetCompactor) RunCompactionSync(ctx context.Context, cfg compaction.TriggerConfig) (compaction.Result, error) {
	c.configs = append(c.configs, cfg)
	if c.started != nil {
		close(c.started)
	}
	if c.delay > 0 {
		select {
		case <-time.After(c.delay):
		case <-ctx.Done():
			return compaction.Result{}, context.Cause(ctx)
		}
	}
	return compaction.Result{Status: c.status}, c.err
}

func discussBudgetRecoveryFixture(t *testing.T) (*Service, *fakeDiscussService, *countingDiscussLifecycleProvider, *discussBudgetCompactor, turn.StartTurnCommand) {
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
	compactor := &discussBudgetCompactor{status: compaction.StatusOK}
	service.compactionService = compactor
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
	return service, resolver, provider, compactor, cmd
}

func TestDiscussProviderAdmissionLeavesFittingHistoryUnchanged(t *testing.T) {
	service, resolver, _, compactor, cmd := discussBudgetRecoveryFixture(t)
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
	for event := range handle.Events() {
		if event.Kind == turn.DiscussEventRecompose {
			t.Error("fitting history requested unnecessary compaction")
		}
	}
	for err := range handle.Errs() {
		t.Errorf("fitting history turn failed: %v", err)
	}
	if len(compactor.configs) != 0 || provider.callCount() != 1 || resolver.storeCalls != 1 {
		t.Fatalf("compaction/provider/history=%d/%d/%d, want 0/1/1 for 50 history tokens within the final 500-token allowance", len(compactor.configs), provider.callCount(), resolver.storeCalls)
	}
	assertSDKMessagesEqual(t, provider.params.Messages, []sdk.Message{sdk.AssistantMessage(history), sdk.UserMessage(current)})
}

func TestDiscussProviderAdmissionNeverWaitsForCompaction(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		t.Run(fmt.Sprintf("overflow=%t", overflow), func(t *testing.T) {
			service, resolver, provider, _, cmd := discussBudgetRecoveryFixture(t)
			configureDiscussLifecycle(service)
			cmd.DiscussMessages = cmd.DiscussMessages[20:]
			if overflow {
				resolver.resolveResult.RunConfig.ContextSourceFrags = []contextfrag.ContextFrag{contextfrag.MessageFrag(contextfrag.MessageFragInput{
					ID: "hook", Message: sdk.UserMessage(strings.Repeat("hook", 800)),
					Kind: contextfrag.KindHookContext, Slot: contextfrag.SlotAfterHistoryBeforeCurrent,
					Budget: contextfrag.BudgetPolicy{Overflow: contextfrag.OverflowKeep},
				})}
			}
			runner := newGatedCompactionRunner()
			defer close(runner.release)
			service.compactionService = runner
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			handle, err := service.StartTurn(ctx, cmd)
			if err != nil {
				t.Fatal(err)
			}
			for event := range handle.Events() {
				if event.Kind == turn.DiscussEventRecompose {
					t.Fatal("admission requested a synchronous retry")
				}
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
	service, resolver, provider, compactor, cmd := discussBudgetRecoveryFixture(t)
	configureDiscussLifecycle(service)
	service.queries.(*controllerQueries).settings.CompactionEnabled = false
	resolver.resolveResult.RunConfig.SupportsImageInput = true
	service.turnHooks.inlineImages = echoDiscussImages
	apply := contextview.ProviderRunConfigApplier(nil)
	service.turnHooks.streamAgent = native.New(native.Deps{ContextViewApplier: func(ctx context.Context, cfg native.RunConfig) (native.RunConfig, error) {
		cfg.RecoverContextBudget = nil
		return apply(ctx, cfg)
	}}).Stream
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
	if provider.callCount() != 1 || countRecoveryText(provider.params.Messages, "older-a ") != 1 || len(compactor.configs) != 0 {
		t.Fatalf("local admission lost text: provider=%d compactions=%d messages=%+v", provider.callCount(), len(compactor.configs), provider.params.Messages)
	}
}
