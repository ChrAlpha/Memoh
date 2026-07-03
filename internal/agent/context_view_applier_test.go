package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/agent/tools"
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
	if len(seen.ContextDynamicMutators) == 0 {
		t.Fatal("applier must see dynamic mutators computed before the view")
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
