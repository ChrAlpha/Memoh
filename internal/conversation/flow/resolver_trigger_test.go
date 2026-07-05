package flow

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/contextview"
)

// triggerCaptureProvider is a minimal sdk.Provider that records the final
// GenerateParams handed to the model, so tests can see exactly what a
// trigger run sends downstream.
type triggerCaptureProvider struct {
	mu     sync.Mutex
	params sdk.GenerateParams
}

func (*triggerCaptureProvider) Name() string { return "trigger-capture" }

func (*triggerCaptureProvider) ListModels(context.Context) ([]sdk.Model, error) { return nil, nil }

func (*triggerCaptureProvider) Test(context.Context) *sdk.ProviderTestResult {
	return &sdk.ProviderTestResult{Status: sdk.ProviderStatusOK}
}

func (*triggerCaptureProvider) TestModel(context.Context, string) (*sdk.ModelTestResult, error) {
	return &sdk.ModelTestResult{Supported: true}, nil
}

func (p *triggerCaptureProvider) DoGenerate(_ context.Context, params sdk.GenerateParams) (*sdk.GenerateResult, error) {
	p.mu.Lock()
	p.params = params
	p.mu.Unlock()
	return &sdk.GenerateResult{Text: "done", FinishReason: sdk.FinishReasonStop}, nil
}

func (p *triggerCaptureProvider) DoStream(ctx context.Context, params sdk.GenerateParams) (*sdk.StreamResult, error) {
	result, err := p.DoGenerate(ctx, params)
	if err != nil {
		return nil, err
	}
	ch := make(chan sdk.StreamPart, 4)
	go func() {
		defer close(ch)
		ch <- &sdk.StartPart{}
		ch <- &sdk.StartStepPart{}
		ch <- &sdk.FinishStepPart{FinishReason: result.FinishReason}
		ch <- &sdk.FinishPart{FinishReason: result.FinishReason}
	}()
	return &sdk.StreamResult{Stream: ch}, nil
}

func (p *triggerCaptureProvider) lastParams() sdk.GenerateParams {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.params
}

// viewRecorder wraps the production context view applier and keeps the
// resulting RunConfig, so tests can inspect ContextFrags/ContextManifest
// classification alongside the rendered messages actually produced.
type viewRecorder struct {
	mu  sync.Mutex
	out agentpkg.RunConfig
}

func (v *viewRecorder) apply(ctx context.Context, cfg agentpkg.RunConfig) agentpkg.RunConfig {
	out := contextview.ApplyProviderRunConfig(ctx, slog.Default(), cfg)
	v.mu.Lock()
	v.out = out
	v.mu.Unlock()
	return out
}

func (v *viewRecorder) snapshot() agentpkg.RunConfig {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.out
}

// triggerResolvedRunConfig mirrors what resolve() (resolver.go) hands back to
// TriggerSchedule/TriggerHeartbeat. usePipeline is always false for these
// callers: each trigger runs in a brand-new session minted by
// sessionCreator.CreateSession (internal/schedule/service.go,
// internal/heartbeat/service.go), which the DCP pipeline has never seen, so
// resolve() always takes its `runCfg.Query = headerifiedQuery` branch.
func triggerResolvedRunConfig(provider sdk.Provider, rawQuery string, now time.Time) agentpkg.RunConfig {
	headerified := FormatUserHeader(UserMessageHeaderInput{
		DisplayName: "User",
		Time:        now,
	}, rawQuery)
	return agentpkg.RunConfig{
		Model:                     &sdk.Model{ID: "trigger-model", Provider: provider, Type: sdk.ModelTypeChat},
		Query:                     headerified,
		ContextScope:              contextfrag.Scope{BotID: "bot-1", ChatID: "bot-1"},
		ContextToolExchangePolicy: &contextfrag.ToolExchangePolicy{MinMessages: 10},
		ContextLifecycle:          contextfrag.NewLifecycleHolder(),
	}
}

func lastMessageText(t *testing.T, messages []sdk.Message) (sdk.MessageRole, string) {
	t.Helper()
	if len(messages) == 0 {
		t.Fatal("expected at least one message")
	}
	last := messages[len(messages)-1]
	if len(last.Content) == 0 {
		t.Fatalf("last message has no content parts: %+v", last)
	}
	text, ok := last.Content[0].(sdk.TextPart)
	if !ok {
		t.Fatalf("expected last message part to be sdk.TextPart, got %T", last.Content[0])
	}
	return last.Role, text.Text
}

func countKind(frags []contextfrag.ContextFrag, kind contextfrag.Kind) int {
	n := 0
	for _, f := range frags {
		if f.Kind == kind {
			n++
		}
	}
	return n
}

func countSlot(frags []contextfrag.ContextFrag, slot contextfrag.Slot) int {
	n := 0
	for _, f := range frags {
		if f.Slot == slot {
			n++
		}
	}
	return n
}

// runTriggerPromptPipeline drives cfg through the same steps TriggerSchedule/
// TriggerHeartbeat run after resolve(): prepareRunConfig, then Agent.Generate
// with the real context view applier wired in. It returns the post-view
// RunConfig (for ContextFrags/ContextManifest assertions) and the params the
// mock model actually received (for a "reached the model" assertion).
func runTriggerPromptPipeline(t *testing.T, cfg agentpkg.RunConfig, provider *triggerCaptureProvider) (agentpkg.RunConfig, sdk.GenerateParams) {
	t.Helper()
	ctx := context.Background()
	r := &Resolver{logger: slog.Default()}
	cfg = r.prepareRunConfig(ctx, cfg)

	recorder := &viewRecorder{}
	a := agentpkg.New(agentpkg.Deps{ContextViewApplier: recorder.apply})
	if _, err := a.Generate(ctx, cfg); err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	return recorder.snapshot(), provider.lastParams()
}

func TestTriggerScheduleAttachesPromptAsCurrentUserMessage(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 5, 9, 0, 0, 0, time.UTC)
	schedulePrompt := agentpkg.GenerateSchedulePrompt(agentpkg.Schedule{
		ID:      "sched-1",
		Name:    "daily digest",
		Command: "summarize inbox",
	}, now)

	provider := &triggerCaptureProvider{}
	cfg := triggerResolvedRunConfig(provider, "summarize inbox", now)
	cfg = attachCurrentTurnPrompt(cfg, schedulePrompt)

	seen, params := runTriggerPromptPipeline(t, cfg, provider)

	if got := countKind(seen.ContextFrags, contextfrag.KindCurrentUserMessage); got != 1 {
		t.Fatalf("expected exactly 1 KindCurrentUserMessage frag, got %d (%+v)", got, seen.ContextFrags)
	}
	if got := countSlot(seen.ContextFrags, contextfrag.SlotHistory); got != 0 {
		t.Fatalf("expected no history-slot frags (schedule prompt must not duplicate as history), got %d (%+v)", got, seen.ContextFrags)
	}

	role, text := lastMessageText(t, seen.Messages)
	if role != sdk.MessageRoleUser || text != schedulePrompt {
		t.Fatalf("expected schedule prompt as last rendered message, got role=%s text=%q", role, text)
	}

	pRole, pText := lastMessageText(t, params.Messages)
	if pRole != sdk.MessageRoleUser || pText != schedulePrompt {
		t.Fatalf("expected model to receive schedule prompt as last user message, got role=%s text=%q", pRole, pText)
	}
}

func TestTriggerHeartbeatAttachesPromptAsCurrentUserMessage(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 5, 9, 0, 0, 0, time.UTC)
	heartbeatPrompt := agentpkg.GenerateHeartbeatPrompt(30, "", now, "")

	provider := &triggerCaptureProvider{}
	cfg := triggerResolvedRunConfig(provider, "heartbeat", now)
	cfg = attachCurrentTurnPrompt(cfg, heartbeatPrompt)

	seen, params := runTriggerPromptPipeline(t, cfg, provider)

	if got := countKind(seen.ContextFrags, contextfrag.KindCurrentUserMessage); got != 1 {
		t.Fatalf("expected exactly 1 KindCurrentUserMessage frag, got %d (%+v)", got, seen.ContextFrags)
	}
	if got := countSlot(seen.ContextFrags, contextfrag.SlotHistory); got != 0 {
		t.Fatalf("expected no history-slot frags (heartbeat prompt must not duplicate as history), got %d (%+v)", got, seen.ContextFrags)
	}

	role, text := lastMessageText(t, seen.Messages)
	if role != sdk.MessageRoleUser || text != heartbeatPrompt {
		t.Fatalf("expected heartbeat prompt as last rendered message, got role=%s text=%q", role, text)
	}

	pRole, pText := lastMessageText(t, params.Messages)
	if pRole != sdk.MessageRoleUser || pText != heartbeatPrompt {
		t.Fatalf("expected model to receive heartbeat prompt as last user message, got role=%s text=%q", pRole, pText)
	}
}
