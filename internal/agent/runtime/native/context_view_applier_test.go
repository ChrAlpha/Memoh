package native

import (
	"context"
	"strings"
	"sync"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	tools "github.com/memohai/memoh/internal/agent/tool"
)

type applierRecorder struct {
	mu     sync.Mutex
	calls  int
	seen   RunConfig
	system string
}

func (r *applierRecorder) applier() ContextViewApplier {
	return func(_ context.Context, cfg RunConfig) RunConfig {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls++
		r.seen = cfg
		if r.system != "" {
			cfg.System = r.system
		}
		return cfg
	}
}

func (r *applierRecorder) snapshot() (int, RunConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.seen
}

func newApplierTestAgent(recorder *applierRecorder, providers ...tools.ToolProvider) *Agent {
	a := New(Deps{ContextViewApplier: recorder.applier()})
	a.SetToolProviders(providers)
	return a
}

func TestGenerateAppliesContextViewAfterToolUsage(t *testing.T) {
	t.Parallel()
	modelProvider := &usageRecordingProvider{}
	recorder := &applierRecorder{system: "VIEW_SYSTEM"}
	a := newApplierTestAgent(recorder, &usageTestProvider{emitTool: true, usage: usageMarker})

	if _, err := a.Generate(context.Background(), RunConfig{
		Model: &sdk.Model{
			ID:       "applier-model",
			Provider: modelProvider,
			Type:     sdk.ModelTypeChat,
		},
		System:           "base system",
		Messages:         []sdk.Message{sdk.UserMessage("hi")},
		SupportsToolCall: true,
	}); err != nil {
		t.Fatalf("Generate error: %v", err)
	}

	calls, seen := recorder.snapshot()
	if calls != 1 {
		t.Fatalf("applier calls = %d, want 1", calls)
	}
	if !strings.Contains(seen.ContextToolUsage, usageMarker) {
		t.Fatalf("applier saw ContextToolUsage %q, want it to contain %q", seen.ContextToolUsage, usageMarker)
	}
	if !strings.Contains(seen.System, usageMarker) {
		t.Fatalf("applier must see the tool-usage-augmented system, got %q", seen.System)
	}
	if params := modelProvider.lastParams(); params.System != "VIEW_SYSTEM" {
		t.Fatalf("model system = %q, want applier output to be authoritative", params.System)
	}
}

func TestGenerateWithoutApplierKeepsLegacyAssembly(t *testing.T) {
	t.Parallel()
	modelProvider := &usageRecordingProvider{}
	a := newTestAgent(&usageTestProvider{emitTool: true, usage: usageMarker})

	if _, err := a.Generate(context.Background(), RunConfig{
		Model: &sdk.Model{
			ID:       "no-applier-model",
			Provider: modelProvider,
			Type:     sdk.ModelTypeChat,
		},
		System:           "base system",
		Messages:         []sdk.Message{sdk.UserMessage("hi")},
		SupportsToolCall: true,
	}); err != nil {
		t.Fatalf("Generate error: %v", err)
	}

	if params := modelProvider.lastParams(); !strings.Contains(params.System, usageMarker) {
		t.Fatalf("legacy assembly must keep tool usage in system, got %q", params.System)
	}
}

type streamStopProvider struct{}

func (*streamStopProvider) Name() string { return "stream-stop" }

func (*streamStopProvider) ListModels(context.Context) ([]sdk.Model, error) { return nil, nil }

func (*streamStopProvider) Test(context.Context) *sdk.ProviderTestResult {
	return &sdk.ProviderTestResult{Status: sdk.ProviderStatusOK}
}

func (*streamStopProvider) TestModel(context.Context, string) (*sdk.ModelTestResult, error) {
	return &sdk.ModelTestResult{Supported: true}, nil
}

func (*streamStopProvider) DoGenerate(context.Context, sdk.GenerateParams) (*sdk.GenerateResult, error) {
	return &sdk.GenerateResult{Text: "ok", FinishReason: sdk.FinishReasonStop}, nil
}

func (*streamStopProvider) DoStream(_ context.Context, _ sdk.GenerateParams) (*sdk.StreamResult, error) {
	ch := make(chan sdk.StreamPart, 4)
	go func() {
		defer close(ch)
		ch <- &sdk.StartPart{}
		ch <- &sdk.StartStepPart{}
		ch <- &sdk.FinishStepPart{FinishReason: sdk.FinishReasonStop}
		ch <- &sdk.FinishPart{FinishReason: sdk.FinishReasonStop}
	}()
	return &sdk.StreamResult{Stream: ch}, nil
}

func TestStreamAppliesContextViewAfterToolUsage(t *testing.T) {
	t.Parallel()
	recorder := &applierRecorder{}
	a := newApplierTestAgent(recorder, &usageTestProvider{emitTool: true, usage: usageMarker})

	events := a.Stream(context.Background(), RunConfig{
		Model: &sdk.Model{
			ID:       "applier-stream-model",
			Provider: &streamStopProvider{},
			Type:     sdk.ModelTypeChat,
		},
		System:           "base system",
		Messages:         []sdk.Message{sdk.UserMessage("hi")},
		SupportsToolCall: true,
	})
	for range events {
	}

	calls, seen := recorder.snapshot()
	if calls != 1 {
		t.Fatalf("applier calls = %d, want 1", calls)
	}
	if !strings.Contains(seen.ContextToolUsage, usageMarker) || !strings.Contains(seen.System, usageMarker) {
		t.Fatalf("stream applier must run after tool usage append, saw system %q", seen.System)
	}
}

func TestSpawnAdapterGoesThroughContextView(t *testing.T) {
	t.Parallel()
	modelProvider := &usageRecordingProvider{}
	recorder := &applierRecorder{}
	a := newApplierTestAgent(recorder)
	adapter := NewSpawnAdapter(a)

	if _, err := adapter.Generate(context.Background(), tools.SpawnRunConfig{
		Model: &sdk.Model{
			ID:       "spawn-model",
			Provider: modelProvider,
			Type:     sdk.ModelTypeChat,
		},
		System: "subagent system",
		Query:  "do the task",
		Identity: tools.SpawnIdentity{
			BotID:      "bot-1",
			SessionID:  "session-1",
			IsSubagent: true,
		},
	}); err != nil {
		t.Fatalf("spawn Generate error: %v", err)
	}

	calls, seen := recorder.snapshot()
	if calls != 1 {
		t.Fatalf("spawn run must go through the context view applier, calls = %d", calls)
	}
	if seen.ContextScope.BotID != "bot-1" || seen.ContextScope.SessionID != "session-1" {
		t.Fatalf("spawn scope not propagated to the view: %+v", seen.ContextScope)
	}
}

func TestGenerateWritesFinalInputHashToManifestLedger(t *testing.T) {
	t.Parallel()
	modelProvider := &usageRecordingProvider{}
	ledger := contextfrag.NewMutationLedger()
	a := New(Deps{ContextViewApplier: func(_ context.Context, cfg RunConfig) RunConfig {
		manifest := contextfrag.Manifest{View: contextfrag.ViewRunConfigPreProvider, Mutations: ledger}
		cfg.ContextManifest = manifest
		cfg.ContextMutations = ledger
		return cfg
	}})

	if _, err := a.Generate(context.Background(), RunConfig{
		Model: &sdk.Model{
			ID:       "lifecycle-model",
			Provider: modelProvider,
			Type:     sdk.ModelTypeChat,
		},
		System:   "base system",
		Messages: []sdk.Message{sdk.UserMessage("hi")},
	}); err != nil {
		t.Fatalf("Generate error: %v", err)
	}

	if ledger.FinalInputHash() == "" {
		t.Fatal("final provider input hash must land on the manifest ledger")
	}
}

func TestGeneratePublishesCacheComparatorPrefixHashAndCacheUsage(t *testing.T) {
	t.Parallel()
	modelProvider := &usageRecordingProvider{usage: sdk.Usage{
		InputTokens: 42,
		InputTokenDetails: sdk.InputTokenDetail{
			CacheReadTokens:  11,
			CacheWriteTokens: 7,
		},
	}}
	holder := contextfrag.NewLifecycleHolder()
	ledger := contextfrag.NewMutationLedger()
	a := New(Deps{ContextViewApplier: func(_ context.Context, cfg RunConfig) RunConfig {
		plan := contextfrag.CachePlan{StablePrefixHash: "fragment-prefix", StableMessageCount: 1}
		manifest := contextfrag.Manifest{
			View:      contextfrag.ViewRunConfigPreProvider,
			CachePlan: &plan,
			Mutations: ledger,
		}
		cfg.ContextCachePlan = plan
		cfg.ContextManifest = manifest
		cfg.ContextMutations = ledger
		if cfg.ContextLifecycle != nil {
			cfg.ContextLifecycle.SetManifest(manifest)
		}
		return cfg
	}})

	if _, err := a.Generate(context.Background(), RunConfig{
		Model: &sdk.Model{
			ID:       "cache-telemetry-model",
			Provider: modelProvider,
			Type:     sdk.ModelTypeChat,
		},
		System:           "stable system",
		Messages:         []sdk.Message{sdk.UserMessage("stable message")},
		ContextLifecycle: holder,
	}); err != nil {
		t.Fatalf("Generate error: %v", err)
	}

	snapshot, ok := holder.Snapshot()
	if !ok {
		t.Fatal("lifecycle snapshot missing")
	}
	if snapshot.CacheComparatorPrefixHash == "" {
		t.Fatalf("cache comparator prefix hash missing: %#v", snapshot)
	}
	if snapshot.CacheReadTokens != 11 || snapshot.CacheWriteTokens != 7 {
		t.Fatalf("cache usage = read %d write %d", snapshot.CacheReadTokens, snapshot.CacheWriteTokens)
	}
}

func TestGenerateComparesPrefixCacheAcrossTurns(t *testing.T) {
	t.Parallel()
	modelProvider := &usageRecordingProvider{usage: sdk.Usage{
		InputTokens:       42,
		InputTokenDetails: sdk.InputTokenDetail{CacheReadTokens: 11},
	}}
	a := New(Deps{ContextViewApplier: contextViewStubApplier})

	runOnce := func() contextfrag.LifecycleSnapshot {
		holder := contextfrag.NewLifecycleHolder()
		if _, err := a.Generate(context.Background(), RunConfig{
			Model: &sdk.Model{
				ID:       "prefix-compare-model",
				Provider: modelProvider,
				Type:     sdk.ModelTypeChat,
			},
			System:           "stable system",
			Messages:         []sdk.Message{sdk.UserMessage("stable message")},
			Identity:         SessionContext{BotID: "bot-1", SessionID: "session-1"},
			ContextLifecycle: holder,
		}); err != nil {
			t.Fatalf("Generate error: %v", err)
		}
		snapshot, ok := holder.Snapshot()
		if !ok {
			t.Fatal("lifecycle snapshot missing")
		}
		return snapshot
	}

	first := runOnce()
	if first.CacheComparison == nil || first.CacheComparison.Outcome != contextfrag.CacheOutcomeFirstObservation {
		t.Fatalf("first turn comparison = %#v, want first observation", first.CacheComparison)
	}
	second := runOnce()
	if second.CacheComparison == nil || second.CacheComparison.Outcome != contextfrag.CacheOutcomeHit {
		t.Fatalf("second turn comparison = %#v, want hit", second.CacheComparison)
	}
	if second.CacheComparison.FirstStepCacheReadTokens != 11 {
		t.Fatalf("first step cache read = %d, want 11", second.CacheComparison.FirstStepCacheReadTokens)
	}
}

func TestGenerateComparesPrefixCacheAcrossModelSwitch(t *testing.T) {
	t.Parallel()
	modelProvider := &usageRecordingProvider{usage: sdk.Usage{
		InputTokens:       42,
		InputTokenDetails: sdk.InputTokenDetail{CacheReadTokens: 11},
	}}
	a := New(Deps{ContextViewApplier: contextViewStubApplier})

	runOnce := func(modelID string) contextfrag.LifecycleSnapshot {
		holder := contextfrag.NewLifecycleHolder()
		if _, err := a.Generate(context.Background(), RunConfig{
			Model: &sdk.Model{
				ID:       modelID,
				Provider: modelProvider,
				Type:     sdk.ModelTypeChat,
			},
			System:           "stable system",
			Messages:         []sdk.Message{sdk.UserMessage("stable message")},
			Identity:         SessionContext{BotID: "bot-switch", SessionID: "session-switch"},
			ContextLifecycle: holder,
		}); err != nil {
			t.Fatalf("Generate error: %v", err)
		}
		snapshot, ok := holder.Snapshot()
		if !ok {
			t.Fatal("lifecycle snapshot missing")
		}
		return snapshot
	}

	first := runOnce("model-a")
	if first.CacheComparison == nil || first.CacheComparison.Outcome != contextfrag.CacheOutcomeFirstObservation {
		t.Fatalf("first turn comparison = %#v, want first observation", first.CacheComparison)
	}
	second := runOnce("model-a")
	if second.CacheComparison == nil || second.CacheComparison.Outcome != contextfrag.CacheOutcomeHit {
		t.Fatalf("second turn (same model) comparison = %#v, want hit", second.CacheComparison)
	}
	third := runOnce("model-b")
	if third.CacheComparison == nil || third.CacheComparison.Outcome != contextfrag.CacheOutcomeModelChanged {
		t.Fatalf("third turn (model switch) comparison = %#v, want model_changed", third.CacheComparison)
	}
	fourth := runOnce("model-a")
	if fourth.CacheComparison == nil || fourth.CacheComparison.Outcome != contextfrag.CacheOutcomeModelChanged {
		t.Fatalf("fourth turn (switch back to model-a) comparison = %#v, want model_changed, not hit", fourth.CacheComparison)
	}
}

func growingPrefixApplier(_ context.Context, cfg RunConfig) RunConfig {
	stable := len(cfg.Messages)
	if stable > 0 {
		stable-- // newest message is the volatile/unstable tail
	}
	plan := contextfrag.CachePlan{StablePrefixHash: "fragment-prefix", StableMessageCount: stable}
	ledger := contextfrag.NewMutationLedger()
	manifest := contextfrag.Manifest{View: contextfrag.ViewRunConfigPreProvider, CachePlan: &plan, Mutations: ledger}
	cfg.ContextCachePlan = plan
	cfg.ContextManifest = manifest
	cfg.ContextMutations = ledger
	if cfg.ContextLifecycle != nil {
		cfg.ContextLifecycle.SetManifest(manifest)
	}
	return cfg
}

func TestGenerateRecognizesPrefixPreservingGrowthAsHit(t *testing.T) {
	t.Parallel()
	modelProvider := &usageRecordingProvider{usage: sdk.Usage{
		InputTokens:       42,
		InputTokenDetails: sdk.InputTokenDetail{CacheReadTokens: 11},
	}}
	a := New(Deps{ContextViewApplier: growingPrefixApplier})

	runOnce := func(messages []sdk.Message) contextfrag.LifecycleSnapshot {
		holder := contextfrag.NewLifecycleHolder()
		if _, err := a.Generate(context.Background(), RunConfig{
			Model: &sdk.Model{
				ID:       "growing-prefix-model",
				Provider: modelProvider,
				Type:     sdk.ModelTypeChat,
			},
			System:           "stable system",
			Messages:         messages,
			Identity:         SessionContext{BotID: "bot-grow", SessionID: "session-grow"},
			ContextLifecycle: holder,
		}); err != nil {
			t.Fatalf("Generate error: %v", err)
		}
		snapshot, ok := holder.Snapshot()
		if !ok {
			t.Fatal("lifecycle snapshot missing")
		}
		return snapshot
	}

	turn1 := []sdk.Message{
		sdk.UserMessage("stable-1"),
		sdk.AssistantMessage("stable-2"),
		sdk.UserMessage("volatile-turn-1"),
	}
	first := runOnce(turn1)
	if first.CacheComparison == nil || first.CacheComparison.Outcome != contextfrag.CacheOutcomeFirstObservation {
		t.Fatalf("first turn comparison = %#v, want first observation", first.CacheComparison)
	}

	// Turn 2 appends two new messages after the byte-identical first three;
	// the stable prefix count grew (2 -> 4) but the previously-cached bytes
	// are unchanged, so this must be a hit, not prefix_changed.
	turn2 := append(append([]sdk.Message(nil), turn1...),
		sdk.AssistantMessage("stable-3"),
		sdk.UserMessage("volatile-turn-2"),
	)
	second := runOnce(turn2)
	if second.CacheComparison == nil || second.CacheComparison.Outcome != contextfrag.CacheOutcomeHit {
		t.Fatalf("second turn (grown, byte-identical prefix) comparison = %#v, want hit", second.CacheComparison)
	}
}

func TestGenerateTreatsEditedGrownPrefixAsPrefixChanged(t *testing.T) {
	t.Parallel()
	modelProvider := &usageRecordingProvider{usage: sdk.Usage{
		InputTokens:       42,
		InputTokenDetails: sdk.InputTokenDetail{CacheReadTokens: 11},
	}}
	a := New(Deps{ContextViewApplier: growingPrefixApplier})

	runOnce := func(messages []sdk.Message) contextfrag.LifecycleSnapshot {
		holder := contextfrag.NewLifecycleHolder()
		if _, err := a.Generate(context.Background(), RunConfig{
			Model: &sdk.Model{
				ID:       "edited-prefix-model",
				Provider: modelProvider,
				Type:     sdk.ModelTypeChat,
			},
			System:           "stable system",
			Messages:         messages,
			Identity:         SessionContext{BotID: "bot-edit", SessionID: "session-edit"},
			ContextLifecycle: holder,
		}); err != nil {
			t.Fatalf("Generate error: %v", err)
		}
		snapshot, ok := holder.Snapshot()
		if !ok {
			t.Fatal("lifecycle snapshot missing")
		}
		return snapshot
	}

	turn1 := []sdk.Message{
		sdk.UserMessage("stable-1"),
		sdk.AssistantMessage("stable-2"),
		sdk.UserMessage("volatile-turn-1"),
	}
	first := runOnce(turn1)
	if first.CacheComparison == nil || first.CacheComparison.Outcome != contextfrag.CacheOutcomeFirstObservation {
		t.Fatalf("first turn comparison = %#v, want first observation", first.CacheComparison)
	}

	// Turn 2 grows the message count, but the first message's TEXT differs
	// from turn 1 — the prefix was actually edited, not just extended, so
	// this must still be prefix_changed.
	turn2 := []sdk.Message{
		sdk.UserMessage("stable-1-edited"),
		sdk.AssistantMessage("stable-2"),
		sdk.UserMessage("volatile-turn-1"),
		sdk.AssistantMessage("stable-3"),
		sdk.UserMessage("volatile-turn-2"),
	}
	second := runOnce(turn2)
	if second.CacheComparison == nil || second.CacheComparison.Outcome != contextfrag.CacheOutcomePrefixChanged {
		t.Fatalf("second turn (grown but edited prefix) comparison = %#v, want prefix_changed", second.CacheComparison)
	}
}

func TestGenerateSkipsPrefixComparisonForSubagents(t *testing.T) {
	t.Parallel()
	modelProvider := &usageRecordingProvider{}
	a := New(Deps{ContextViewApplier: contextViewStubApplier})
	holder := contextfrag.NewLifecycleHolder()

	if _, err := a.Generate(context.Background(), RunConfig{
		Model: &sdk.Model{
			ID:       "subagent-model",
			Provider: modelProvider,
			Type:     sdk.ModelTypeChat,
		},
		System:           "subagent system",
		Messages:         []sdk.Message{sdk.UserMessage("task")},
		Identity:         SessionContext{BotID: "bot-1", SessionID: "session-1", IsSubagent: true},
		ContextLifecycle: holder,
	}); err != nil {
		t.Fatalf("Generate error: %v", err)
	}

	snapshot, ok := holder.Snapshot()
	if !ok {
		t.Fatal("lifecycle snapshot missing")
	}
	if snapshot.CacheComparison != nil {
		t.Fatalf("subagent runs must not join the session prefix comparison: %#v", snapshot.CacheComparison)
	}
}

func contextViewStubApplier(_ context.Context, cfg RunConfig) RunConfig {
	plan := contextfrag.CachePlan{StablePrefixHash: "fragment-prefix", StableMessageCount: 0}
	ledger := contextfrag.NewMutationLedger()
	manifest := contextfrag.Manifest{
		View:      contextfrag.ViewRunConfigPreProvider,
		CachePlan: &plan,
		Mutations: ledger,
	}
	cfg.ContextCachePlan = plan
	cfg.ContextManifest = manifest
	cfg.ContextMutations = ledger
	if cfg.ContextLifecycle != nil {
		cfg.ContextLifecycle.SetManifest(manifest)
	}
	return cfg
}

func TestGenerateFragsFirstHandsToolUsageToView(t *testing.T) {
	t.Parallel()
	modelProvider := &usageRecordingProvider{}
	recorder := &applierRecorder{}
	a := newApplierTestAgent(recorder, &usageTestProvider{emitTool: true, usage: usageMarker})

	sourceFrag := contextfrag.TextFrag(contextfrag.TextFragInput{
		ID:    "system.prompt",
		Kind:  contextfrag.KindSystemPrompt,
		Slot:  contextfrag.SlotSystem,
		Text:  "frag system",
		Scope: contextfrag.Scope{BotID: "bot-1"},
	})
	if _, err := a.Generate(context.Background(), RunConfig{
		Model: &sdk.Model{
			ID:       "frags-first-model",
			Provider: modelProvider,
			Type:     sdk.ModelTypeChat,
		},
		System:             "base system",
		Messages:           []sdk.Message{sdk.UserMessage("hi")},
		SupportsToolCall:   true,
		ContextSourceFrags: []contextfrag.ContextFrag{sourceFrag},
	}); err != nil {
		t.Fatalf("Generate error: %v", err)
	}

	_, seen := recorder.snapshot()
	if seen.System != "base system" {
		t.Fatalf("frags-first system must stay untouched by tool usage append, got %q", seen.System)
	}
	if !strings.Contains(seen.ContextToolUsage, usageMarker) {
		t.Fatalf("tool usage must reach the view, got %q", seen.ContextToolUsage)
	}
	if len(seen.ContextSourceFrags) != 1 {
		t.Fatalf("source frags must ride through, got %d", len(seen.ContextSourceFrags))
	}
}
