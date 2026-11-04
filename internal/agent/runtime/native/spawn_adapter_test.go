package native

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	tools "github.com/felinics/memoh/internal/agent/tool"
)

// renderTask arms rc with the task as the run's only message, the way a
// binder renders a thread whose history is just the task.
func renderTask(task tools.SpawnRunConfig, rc *RunConfig) {
	rc.Messages = []sdk.Message{sdk.UserMessage(task.Query)}
	current := 0
	rc.ContextCurrentUserMessageIndex = &current
	rc.ContextQueryMaterialized = true
}

// taskBinder is a binder for tests about how a spawned run streams and ends
// rather than what it reads: it renders the task and closes nothing.
func taskBinder(_ context.Context, task tools.SpawnRunConfig, rc *RunConfig, _ *tools.SubagentWatchdog) (func(context.Context) error, error) {
	renderTask(task, rc)
	return func(context.Context) error { return nil }, nil
}

// newTaskAdapter is a spawn adapter armed by taskBinder.
func newTaskAdapter(a *Agent) *SpawnAdapter {
	adapter := NewSpawnAdapter(a)
	adapter.SetRunBinder(taskBinder)
	return adapter
}

// testWatchdog is a watchdog that never fires within a test.
func testWatchdog(t *testing.T) *tools.SubagentWatchdog {
	t.Helper()
	_, watchdog := tools.NewSubagentWatchdog(context.Background(), time.Hour, slog.New(slog.DiscardHandler))
	t.Cleanup(watchdog.Stop)
	return watchdog
}

func TestSpawnRunConfigCarriesContextBudget(t *testing.T) {
	t.Parallel()

	rc := runConfigFromSpawnRunConfig(tools.SpawnRunConfig{
		ContextBudgetMaxTokens: 128000,
	})

	if rc.ContextBudgetMaxTokens != 128000 {
		t.Fatalf("ContextBudgetMaxTokens = %d, want 128000", rc.ContextBudgetMaxTokens)
	}
}

// The adapter builds no context of its own: a spawned run reads what its
// binder renders, and an adapter without one fails before the provider.
func TestSpawnAdapterWithoutBinderFailsBeforeTheProvider(t *testing.T) {
	t.Parallel()

	rc := runConfigFromSpawnRunConfig(tools.SpawnRunConfig{Query: "do the task"})
	if len(rc.Messages) != 0 || rc.Query != "" || rc.ContextCurrentUserMessageIndex != nil {
		t.Fatalf("base run config renders messages=%d query=%q current=%v, want nothing before the binder", len(rc.Messages), rc.Query, rc.ContextCurrentUserMessageIndex)
	}
	provider := &preflightCountingProvider{}
	outcome := tools.SpawnAttemptCompleted
	_, err := NewSpawnAdapter(New(Deps{})).GenerateWithWatchdog(context.Background(), tools.SpawnRunConfig{
		Model:             &sdk.Model{ID: "spawn-unbound", Provider: provider},
		Query:             "do the task",
		ResolveAttempt:    func(error) tools.SpawnAttemptDisposition { return tools.SpawnAttemptFailure },
		ReconcileTerminal: func(o tools.SpawnAttemptDisposition) { outcome = o },
	}, testWatchdog(t))
	if !errors.Is(err, errSpawnRunUnbound) || outcome != tools.SpawnAttemptFailure {
		t.Fatalf("unbound run: err=%v outcome=%v, want the binder's absence as the attempt's failure", err, outcome)
	}
	if generateCalls, streamCalls := provider.calls(); generateCalls != 0 || streamCalls != 0 {
		t.Fatalf("provider calls = generate:%d stream:%d, want none", generateCalls, streamCalls)
	}
}

// A binder that cannot arm the run fails the attempt with its error, which
// the run's observer publishes as it publishes a stream failure.
func TestSpawnAdapterPublishesABindFailure(t *testing.T) {
	t.Parallel()

	bindErr := errors.New("history unavailable")
	adapter := NewSpawnAdapter(New(Deps{}))
	adapter.SetRunBinder(func(context.Context, tools.SpawnRunConfig, *RunConfig, *tools.SubagentWatchdog) (func(context.Context) error, error) {
		return nil, bindErr
	})
	var published []StreamEvent
	adapter.SetRunObserverFactory(func(context.Context) SpawnRunObserver {
		return func(event StreamEvent) SpawnRunObservation {
			published = append(published, event)
			return SpawnRunObservation{}
		}
	})
	provider := &preflightCountingProvider{}
	_, err := adapter.GenerateWithWatchdog(context.Background(), tools.SpawnRunConfig{
		Model:          &sdk.Model{ID: "spawn-bind-failure", Provider: provider},
		Query:          "do the task",
		ResolveAttempt: func(error) tools.SpawnAttemptDisposition { return tools.SpawnAttemptFailure },
	}, testWatchdog(t))
	if !errors.Is(err, bindErr) {
		t.Fatalf("err = %v, want the bind failure", err)
	}
	if len(published) != 1 || published[0].Type != EventError || !errors.Is(published[0].Cause, bindErr) {
		t.Fatalf("published = %#v, want the bind failure as the run's error", published)
	}
	if generateCalls, streamCalls := provider.calls(); generateCalls != 0 || streamCalls != 0 {
		t.Fatalf("provider calls = generate:%d stream:%d, want none", generateCalls, streamCalls)
	}
}

func TestSpawnAdapterGenerateWithWatchdogFailsOnStreamError(t *testing.T) {
	t.Parallel()

	plan := contextfrag.ContextBudgetPlan{
		Window:           4096,
		OutputReserve:    8192,
		ActualSystemCost: 123,
	}
	a := New(Deps{
		ContextViewApplier: spawnFailingBudgetApplier(plan, contextfrag.ErrBudgetUnsatisfied),
	})
	adapter := NewSpawnAdapter(a)
	adapter.SetRunBinder(taskBinder)
	provider := &preflightCountingProvider{}

	result, err := adapter.GenerateWithWatchdog(context.Background(), tools.SpawnRunConfig{
		Model: &sdk.Model{
			ID:       "spawn-preflight-error",
			Provider: provider,
		},
		Query: "do the task",
	}, testWatchdog(t))

	if result == nil || result.ContextLifecycle == nil {
		t.Fatalf("GenerateWithWatchdog result = %#v, want failure lifecycle audit", result)
	}
	if got := result.ContextLifecycle.BudgetPlan; got == nil || !reflect.DeepEqual(*got, plan) {
		t.Fatalf("failure lifecycle budget plan = %#v, want %#v", got, plan)
	}
	if !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) {
		t.Fatalf("GenerateWithWatchdog error = %v, want the context-budget failure", err)
	}
	if generateCalls, streamCalls := provider.calls(); generateCalls != 0 || streamCalls != 0 {
		t.Fatalf("provider calls = generate:%d stream:%d, want zero after preflight failure", generateCalls, streamCalls)
	}
}

func spawnFailingBudgetApplier(plan contextfrag.ContextBudgetPlan, err error) ContextViewApplier {
	return func(_ context.Context, cfg RunConfig) (RunConfig, error) {
		manifest := contextfrag.Manifest{
			View:       contextfrag.ViewRunConfigPreProvider,
			BudgetPlan: &plan,
		}
		cfg.ContextManifest = manifest
		if cfg.ContextLifecycle != nil {
			cfg.ContextLifecycle.SetManifest(manifest)
		}
		return cfg, err
	}
}
