package native

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	agentevent "github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/agent/step"
	tools "github.com/felinics/memoh/internal/agent/tool"
)

func TestSpawnAdapterGenerateWithWatchdogKeepsRetryableAttemptNonTerminal(t *testing.T) {
	causes := []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		err  error
	}{
		{
			name: "watchdog",
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancelCause(context.Background())
				cancel(tools.ErrWatchdogTimedOut)
				return ctx, func() {}
			},
			err: tools.ErrWatchdogTimedOut,
		},
		{
			name: "safety deadline",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
			err: context.DeadlineExceeded,
		},
	}
	for _, tc := range causes {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.ctx()
			defer cancel()
			provider := &atomicMockProvider{
				handler: func(_ int, _ sdk.Request) (sdk.ModelResult, error) {
					return sdk.ModelResult{FinishReason: sdk.FinishReasonStop}, nil
				},
			}
			adapter := newCommittingSpawnAdapter()
			var observed []StreamEvent
			adapter.SetRunObserverFactory(func(context.Context) SpawnRunObserver {
				return func(event StreamEvent) SpawnRunObservation {
					observed = append(observed, event)
					return SpawnRunObservation{}
				}
			})
			_, err := adapter.GenerateWithWatchdog(ctx, tools.SpawnRunConfig{
				Model:       &sdk.Model{ID: "spawn-retry-model", Provider: provider, Type: sdk.ModelTypeChat},
				Query:       "retry the task",
				SessionType: sessionmode.Subagent,
				Identity:    tools.SpawnIdentity{BotID: "bot-1", SessionID: "session-1", IsSubagent: true},
				Attempt:     1,
				MaxAttempts: 4,
				ResolveAttempt: func(got error) tools.SpawnAttemptDisposition {
					if errors.Is(got, tc.err) {
						return tools.SpawnAttemptRetry
					}
					return tools.SpawnAttemptFailure
				},
			}, func() {})
			if !errors.Is(err, tc.err) {
				t.Fatalf("GenerateWithWatchdog error = %v, want %v", err, tc.err)
			}
			assertSpawnAttemptObservedAsRetry(t, observed, 1, 4)
		})
	}
}

func TestSpawnAdapterGenerateWithWatchdogMakesFinalWatchdogAttemptTerminal(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(tools.ErrWatchdogTimedOut)
	provider := &atomicMockProvider{
		handler: func(_ int, _ sdk.Request) (sdk.ModelResult, error) {
			return sdk.ModelResult{FinishReason: sdk.FinishReasonStop}, nil
		},
	}
	adapter := newCommittingSpawnAdapter()
	var observed []StreamEvent
	var dispositionCalls int
	adapter.SetRunObserverFactory(func(context.Context) SpawnRunObserver {
		return func(event StreamEvent) SpawnRunObservation {
			observed = append(observed, event)
			return SpawnRunObservation{}
		}
	})
	_, err := adapter.GenerateWithWatchdog(ctx, tools.SpawnRunConfig{
		Model:       &sdk.Model{ID: "spawn-final-watchdog-model", Provider: provider, Type: sdk.ModelTypeChat},
		Query:       "finish the task",
		SessionType: sessionmode.Subagent,
		Identity:    tools.SpawnIdentity{BotID: "bot-1", SessionID: "session-1", IsSubagent: true},
		Attempt:     4,
		MaxAttempts: 4,
		ResolveAttempt: func(got error) tools.SpawnAttemptDisposition {
			dispositionCalls++
			if !errors.Is(got, tools.ErrWatchdogTimedOut) {
				t.Errorf("attempt error = %v, want watchdog timeout", got)
			}
			return tools.SpawnAttemptFailure
		},
	}, func() {})
	if !errors.Is(err, tools.ErrWatchdogTimedOut) {
		t.Fatalf("GenerateWithWatchdog error = %v, want watchdog timeout", err)
	}
	if dispositionCalls != 1 {
		t.Fatalf("attempt resolver calls = %d, want 1", dispositionCalls)
	}
	assertSpawnAbortObservedAsFailure(t, observed)
	for _, event := range observed {
		if event.Type == EventRetry {
			t.Fatalf("final watchdog attempt published retry: %#v", observed)
		}
	}
}

func TestSpawnAdapterGenerateWithWatchdogDoesNotRetryPersistedInterruptedCheckpoint(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	provider := &atomicMockProvider{
		stream: func(streamCtx context.Context, _ sdk.Request) (<-chan sdk.StreamPart, error) {
			parts := make(chan sdk.StreamPart)
			go func() {
				defer close(parts)
				for _, part := range []sdk.StreamPart{
					&sdk.StartPart{},
					&sdk.StartStepPart{},
					&sdk.TextStartPart{ID: "partial"},
					&sdk.TextDeltaPart{ID: "partial", Text: "durable partial"},
				} {
					select {
					case <-streamCtx.Done():
						return
					case parts <- part:
					}
				}
				<-streamCtx.Done()
			}()
			return parts, nil
		},
	}
	adapter := newCommittingSpawnAdapter()
	var persisted atomic.Bool
	var interruptedCalls atomic.Int32
	adapter.SetStepCommitFactory(func(
		_ context.Context,
		_ string,
		_ string,
		_ string,
		_ string,
		_ *contextfrag.LifecycleHolder,
		onPersisted func(),
	) (func(context.Context, int, *step.Record) error, func(context.Context, int, *step.Record) error, error) {
		return func(context.Context, int, *step.Record) error { return nil },
			func(context.Context, int, *step.Record) error {
				interruptedCalls.Add(1)
				onPersisted()
				return nil
			}, nil
	})
	var observed []StreamEvent
	adapter.SetRunObserverFactory(func(context.Context) SpawnRunObserver {
		return func(event StreamEvent) SpawnRunObservation {
			observed = append(observed, event)
			return SpawnRunObservation{}
		}
	})
	var touches atomic.Int32
	_, err := adapter.GenerateWithWatchdog(ctx, tools.SpawnRunConfig{
		Model:       &sdk.Model{ID: "spawn-interrupted-model", Provider: provider, Type: sdk.ModelTypeChat},
		Query:       "preserve interrupted output",
		SessionType: sessionmode.Subagent,
		Identity:    tools.SpawnIdentity{BotID: "bot-1", SessionID: "session-1", IsSubagent: true},
		Attempt:     1,
		MaxAttempts: 4,
		OnStepPersisted: func() {
			persisted.Store(true)
		},
		ResolveAttempt: func(error) tools.SpawnAttemptDisposition {
			if persisted.Load() {
				return tools.SpawnAttemptFailure
			}
			return tools.SpawnAttemptRetry
		},
	}, func() {
		if touches.Add(1) == 3 {
			cancel(tools.ErrWatchdogTimedOut)
		}
	})
	if !errors.Is(err, tools.ErrWatchdogTimedOut) {
		t.Fatalf("GenerateWithWatchdog error = %v, want watchdog timeout", err)
	}
	if interruptedCalls.Load() != 1 || !persisted.Load() {
		t.Fatalf("interrupted persistence = calls %d, persisted %v", interruptedCalls.Load(), persisted.Load())
	}
	assertSpawnAbortObservedAsFailure(t, observed)
	for _, event := range observed {
		if event.Type == EventRetry {
			t.Fatalf("persisted interrupted attempt published retry: %#v", observed)
		}
	}
}

// assertSpawnAttemptObservedAsRetry requires the session to see an attempt the
// spawn provider runs again as one retry and nothing else: no error, no abort.
func assertSpawnAttemptObservedAsRetry(t *testing.T, events []StreamEvent, attempt, maxAttempts int) {
	t.Helper()
	retries := 0
	for _, event := range events {
		switch event.Type {
		case EventError:
			t.Fatalf("retryable attempt published an error: %#v", events)
		case EventAgentAbort:
			t.Fatalf("retryable attempt published terminal abort: %#v", events)
		case EventRetry:
			if event.Attempt != attempt || event.MaxAttempt != maxAttempts {
				t.Fatalf("retry event = %#v, want attempt %d of %d", event, attempt, maxAttempts)
			}
			retries++
		}
	}
	if retries != 1 {
		t.Fatalf("observed events = %#v, want exactly one retry", events)
	}
}

// An attempt that ends with a provider failure publishes that failure only
// when it is the run's outcome; an attempt that is run again publishes the
// retry alone.
func TestObserveSpawnAttemptFailurePublishesOnlyTheFinalFailure(t *testing.T) {
	providerErr := &sdk.APIError{StatusCode: 500, Kind: sdk.KindServerError}
	abort := &StreamEvent{Type: EventAgentAbort}
	tests := []struct {
		name        string
		disposition tools.SpawnAttemptDisposition
		want        []agentevent.StreamEventType
	}{
		{name: "retry", disposition: tools.SpawnAttemptRetry, want: []agentevent.StreamEventType{EventRetry}},
		{name: "final", disposition: tools.SpawnAttemptFailure, want: []agentevent.StreamEventType{EventError, EventAgentAbort}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var observed []StreamEvent
			observeSpawnAttemptFailure(
				context.Background(),
				func(event StreamEvent) SpawnRunObservation {
					observed = append(observed, event)
					return SpawnRunObservation{}
				},
				tools.SpawnRunConfig{
					Attempt:        1,
					MaxAttempts:    2,
					ResolveAttempt: func(error) tools.SpawnAttemptDisposition { return tc.disposition },
				},
				abort,
				[]StreamEvent{{Type: EventError, Cause: providerErr}},
				providerErr,
			)
			if len(observed) != len(tc.want) {
				t.Fatalf("observed events = %#v, want %v", observed, tc.want)
			}
			for i, event := range observed {
				if event.Type != tc.want[i] {
					t.Fatalf("observed events = %#v, want %v", observed, tc.want)
				}
			}
			switch observed[0].Type {
			case EventRetry:
				if observed[0].Attempt != 1 || observed[0].MaxAttempt != 2 || observed[0].Error != "" || observed[0].Cause != nil {
					t.Fatalf("retry event = %#v, want the attempt counters alone", observed[0])
				}
			case EventError:
				var apiErr *sdk.APIError
				if !errors.As(observed[0].Cause, &apiErr) || apiErr != providerErr {
					t.Fatalf("error event cause = %v, want the provider failure", observed[0].Cause)
				}
			}
		})
	}
}

func TestObserveSpawnAttemptFailureMakesOwningCancellationAuthoritative(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(context.Canceled)
	abort := &StreamEvent{
		Type:     EventAgentAbort,
		Messages: []byte(`[{"role":"assistant"}]`),
		Usage:    []byte(`{"output_tokens":7}`),
	}
	var observed []StreamEvent
	var dispositionCalls int
	observeSpawnAttemptFailure(
		ctx,
		func(event StreamEvent) SpawnRunObservation {
			observed = append(observed, event)
			return SpawnRunObservation{}
		},
		tools.SpawnRunConfig{ResolveAttempt: func(error) tools.SpawnAttemptDisposition {
			dispositionCalls++
			return tools.SpawnAttemptAbort
		}},
		abort,
		[]StreamEvent{{Type: EventError, Cause: errors.New("private provider detail")}},
		context.Canceled,
	)
	if dispositionCalls != 1 {
		t.Fatalf("attempt resolver calls = %d, want 1", dispositionCalls)
	}
	if len(observed) != 1 || observed[0].Type != EventAgentAbort ||
		string(observed[0].Messages) != string(abort.Messages) || string(observed[0].Usage) != string(abort.Usage) {
		t.Fatalf("observed events = %#v, want exact abort without raced error", observed)
	}
}

func TestObserveSpawnAttemptFailureResolvesWithoutObserver(t *testing.T) {
	var calls int
	observeSpawnAttemptFailure(
		context.Background(),
		nil,
		tools.SpawnRunConfig{ResolveAttempt: func(error) tools.SpawnAttemptDisposition {
			calls++
			return tools.SpawnAttemptFailure
		}},
		nil,
		nil,
		errors.New("failed before streaming"),
	)
	if calls != 1 {
		t.Fatalf("attempt resolver calls = %d, want 1 without observer", calls)
	}
}

func TestSpawnAdapterArbitratesCleanEndBeforePublishingTerminal(t *testing.T) {
	tests := []struct {
		name        string
		disposition tools.SpawnAttemptDisposition
		wantErr     error
		wantEvent   agentevent.StreamEventType
	}{
		{
			name:        "completed",
			disposition: tools.SpawnAttemptCompleted,
			wantEvent:   EventAgentEnd,
		},
		{
			name:        "stop won",
			disposition: tools.SpawnAttemptAbort,
			wantErr:     context.Canceled,
			wantEvent:   EventAgentAbort,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			provider := &atomicMockProvider{
				handler: func(_ int, _ sdk.Request) (sdk.ModelResult, error) {
					return sdk.ModelResult{Text: "done", FinishReason: sdk.FinishReasonStop}, nil
				},
			}
			adapter := newCommittingSpawnAdapter()
			var observed []StreamEvent
			adapter.SetRunObserverFactory(func(context.Context) SpawnRunObserver {
				return func(event StreamEvent) SpawnRunObservation {
					observed = append(observed, event)
					return SpawnRunObservation{}
				}
			})
			var resolutionCalls int
			_, err := adapter.GenerateWithWatchdog(context.Background(), tools.SpawnRunConfig{
				Model:       &sdk.Model{ID: "spawn-terminal-model", Provider: provider, Type: sdk.ModelTypeChat},
				Query:       "finish the task",
				SessionType: sessionmode.Subagent,
				Identity:    tools.SpawnIdentity{BotID: "bot-1", SessionID: "session-1", IsSubagent: true},
				ResolveCompletion: func() tools.SpawnAttemptDisposition {
					resolutionCalls++
					return tc.disposition
				},
			}, func() {})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("GenerateWithWatchdog error = %v, want %v", err, tc.wantErr)
			}
			if resolutionCalls != 1 {
				t.Fatalf("completion resolver calls = %d, want 1", resolutionCalls)
			}
			var terminals []agentevent.StreamEventType
			for _, event := range observed {
				if event.Type == EventAgentEnd || event.Type == EventAgentAbort {
					terminals = append(terminals, event.Type)
				}
			}
			if len(terminals) != 1 || terminals[0] != tc.wantEvent {
				t.Fatalf("terminal events = %v, want only %s", terminals, tc.wantEvent)
			}
		})
	}
}

// A spawned run's history is written by its step commits, which finish before
// the run's terminal event is published.
func TestSpawnAdapterCommitsStepsBeforePublishingTerminal(t *testing.T) {
	provider := &atomicMockProvider{
		handler: func(_ int, _ sdk.Request) (sdk.ModelResult, error) {
			return sdk.ModelResult{Text: "done", FinishReason: sdk.FinishReasonStop}, nil
		},
	}
	adapter := NewSpawnAdapter(newTestAgent())
	committed := false
	terminalBeforeCommit := false
	adapter.SetStepCommitFactory(func(
		context.Context, string, string, string, string, *contextfrag.LifecycleHolder, func(),
	) (func(context.Context, int, *step.Record) error, func(context.Context, int, *step.Record) error, error) {
		commit := func(context.Context, int, *step.Record) error {
			committed = true
			return nil
		}
		return commit, commit, nil
	})
	adapter.SetRunObserverFactory(func(context.Context) SpawnRunObserver {
		return func(event StreamEvent) SpawnRunObservation {
			if event.Type == EventAgentEnd && !committed {
				terminalBeforeCommit = true
			}
			return SpawnRunObservation{}
		}
	})

	if _, err := adapter.GenerateWithWatchdog(context.Background(), tools.SpawnRunConfig{
		Model:       &sdk.Model{ID: "spawn-terminal-barrier", Provider: provider, Type: sdk.ModelTypeChat},
		Query:       "finish the task",
		SessionType: sessionmode.Subagent,
		Identity:    tools.SpawnIdentity{BotID: "bot-1", SessionID: "session-1", IsSubagent: true},
	}, func() {}); err != nil {
		t.Fatalf("GenerateWithWatchdog() error = %v", err)
	}
	if !committed || terminalBeforeCommit {
		t.Fatalf("committed = %v, terminal before the commit = %v", committed, terminalBeforeCommit)
	}
}

// A step whose history write fails ends the attempt as a failure and publishes
// no terminal.
func TestSpawnAdapterWithholdsTerminalWhenStepCommitFails(t *testing.T) {
	provider := &atomicMockProvider{
		handler: func(_ int, _ sdk.Request) (sdk.ModelResult, error) {
			return sdk.ModelResult{Text: "done", FinishReason: sdk.FinishReasonStop}, nil
		},
	}
	adapter := NewSpawnAdapter(newTestAgent())
	persistErr := errors.New("history unavailable")
	adapter.SetStepCommitFactory(func(
		context.Context, string, string, string, string, *contextfrag.LifecycleHolder, func(),
	) (func(context.Context, int, *step.Record) error, func(context.Context, int, *step.Record) error, error) {
		commit := func(context.Context, int, *step.Record) error { return persistErr }
		return commit, commit, nil
	})
	var terminalSeen bool
	adapter.SetRunObserverFactory(func(context.Context) SpawnRunObserver {
		return func(event StreamEvent) SpawnRunObservation {
			terminalSeen = terminalSeen || event.Type == EventAgentEnd
			return SpawnRunObservation{}
		}
	})

	_, err := adapter.GenerateWithWatchdog(context.Background(), tools.SpawnRunConfig{
		Model:       &sdk.Model{ID: "spawn-terminal-barrier-failure", Provider: provider, Type: sdk.ModelTypeChat},
		Query:       "finish the task",
		SessionType: sessionmode.Subagent,
		Identity:    tools.SpawnIdentity{BotID: "bot-1", SessionID: "session-1", IsSubagent: true},
	}, func() {})
	if !errors.Is(err, persistErr) {
		t.Fatalf("GenerateWithWatchdog() error = %v, want the step write failure", err)
	}
	if terminalSeen {
		t.Fatal("terminal event was published after the step write failed")
	}
}

// A run that cannot commit its steps does not start: the factory's refusal is
// the run's error and the model is never called.
func TestSpawnAdapterRefusesARunWhoseStepCommitIsRefused(t *testing.T) {
	var calls atomic.Int32
	provider := &atomicMockProvider{
		handler: func(int, sdk.Request) (sdk.ModelResult, error) {
			calls.Add(1)
			return sdk.ModelResult{Text: "done", FinishReason: sdk.FinishReasonStop}, nil
		},
	}
	refused := errors.New("no request row")
	for name, adapter := range map[string]*SpawnAdapter{
		"factory refuses": func() *SpawnAdapter {
			a := NewSpawnAdapter(newTestAgent())
			a.SetStepCommitFactory(func(
				context.Context, string, string, string, string, *contextfrag.LifecycleHolder, func(),
			) (func(context.Context, int, *step.Record) error, func(context.Context, int, *step.Record) error, error) {
				return nil, nil, refused
			})
			return a
		}(),
		"no factory": NewSpawnAdapter(newTestAgent()),
	} {
		cfg := tools.SpawnRunConfig{
			Model:       &sdk.Model{ID: "spawn-refused", Provider: provider, Type: sdk.ModelTypeChat},
			Query:       "finish the task",
			SessionType: sessionmode.Subagent,
			Identity:    tools.SpawnIdentity{BotID: "bot-1", SessionID: "session-1", IsSubagent: true},
		}
		if _, err := adapter.GenerateWithWatchdog(context.Background(), cfg, func() {}); err == nil {
			t.Fatalf("%s: GenerateWithWatchdog started the run", name)
		}
		if _, err := adapter.Generate(context.Background(), cfg); err == nil {
			t.Fatalf("%s: Generate started the run", name)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("model called %d times for refused runs", calls.Load())
	}
}

// newCommittingSpawnAdapter is an adapter whose steps commit to nowhere.
func newCommittingSpawnAdapter() *SpawnAdapter {
	adapter := NewSpawnAdapter(newTestAgent())
	adapter.SetStepCommitFactory(func(
		context.Context, string, string, string, string, *contextfrag.LifecycleHolder, func(),
	) (func(context.Context, int, *step.Record) error, func(context.Context, int, *step.Record) error, error) {
		commit := func(context.Context, int, *step.Record) error { return nil }
		return commit, commit, nil
	})
	return adapter
}
