package native

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/event"
)

// streamTerminal drives one run and returns its single terminal event. Every
// case below asserts on the terminal this helper returns, so a run that ends
// twice or not at all fails the case it belongs to.
func streamTerminal(ctx context.Context, t *testing.T, a *Agent, cfg RunConfig) StreamEvent {
	t.Helper()

	var (
		terminal StreamEvent
		seen     int
	)
	for ev := range a.Stream(ctx, cfg) {
		if ev.IsTerminal() {
			terminal = ev
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("terminal events = %d, want exactly 1", seen)
	}
	return terminal
}

func assertEndReason(t *testing.T, terminal StreamEvent, wantType StreamEventType, wantReason event.EndReason) {
	t.Helper()

	if terminal.Type != wantType {
		t.Fatalf("terminal event = %q (%s), want %q", terminal.Type, terminal.Error, wantType)
	}
	if terminal.EndReason != wantReason {
		t.Fatalf("terminal EndReason = %q, want %q", terminal.EndReason, wantReason)
	}
}

func TestAgentStreamEndReasonCompleted(t *testing.T) {
	t.Parallel()

	terminal := streamTerminal(context.Background(), t, New(Deps{}), RunConfig{
		Model:    &sdk.Model{ID: "mock-model", Provider: finishedTextTestProvider("done")},
		Messages: []sdk.Message{sdk.UserMessage("task")},
		Identity: SessionContext{BotID: "bot-1"},
	})
	assertEndReason(t, terminal, EventAgentEnd, event.EndCompleted)
}

func TestAgentStreamEndReasonAwaitingDecision(t *testing.T) {
	t.Parallel()

	a, provider, _, _ := deferredApprovalBatch(t)
	terminal := streamTerminal(context.Background(), t, a, RunConfig{
		Model:               &sdk.Model{ID: "mock-model", Provider: provider},
		Messages:            []sdk.Message{sdk.UserMessage("start")},
		SupportsToolCall:    true,
		Identity:            SessionContext{BotID: "bot-1"},
		ToolApprovalHandler: deferOnExec,
	})
	assertEndReason(t, terminal, EventAgentEnd, event.EndAwaitingDecision)
	if terminal.ApprovalID == "" {
		t.Fatal("terminal event carries no approval id for a parked decision")
	}
}

// The run's own cancellation carries no more specific reason, so the terminal
// reports the cancellation itself rather than an ending the loop did not have.
func TestAgentStreamEndReasonCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{})
	var enteredOnce sync.Once
	provider := agentStreamTestProvider(func(streamCtx context.Context, _ sdk.Request) (<-chan sdk.StreamPart, error) {
		parts := make(chan sdk.StreamPart, 2)
		parts <- &sdk.StartPart{}
		parts <- &sdk.StartStepPart{}
		enteredOnce.Do(func() { close(entered) })
		go func() {
			<-streamCtx.Done()
			close(parts)
		}()
		return parts, nil
	})
	go func() {
		<-entered
		cancel()
	}()

	terminal := streamTerminal(ctx, t, New(Deps{}), RunConfig{
		Model:    &sdk.Model{ID: "mock-model", Provider: provider},
		Messages: []sdk.Message{sdk.UserMessage("task")},
		Identity: SessionContext{BotID: "bot-1"},
	})
	assertEndReason(t, terminal, EventAgentAbort, event.EndCanceled)
}

func TestAgentStreamEndReasonFailed(t *testing.T) {
	t.Parallel()

	provider := agentStreamTestProvider(func(context.Context, sdk.Request) (<-chan sdk.StreamPart, error) {
		return nil, rejectedErr()
	})
	terminal := streamTerminal(context.Background(), t, New(Deps{}), RunConfig{
		Model:            &sdk.Model{ID: "mock-model", Provider: provider},
		Messages:         []sdk.Message{sdk.UserMessage("task")},
		Identity:         SessionContext{BotID: "bot-1"},
		ContextMutations: contextfrag.NewMutationLedger(),
	})
	assertEndReason(t, terminal, EventAgentAbort, event.EndFailed)
}

// A step whose context does not fit ends the run on the budget, not on the
// cancellation that reports it. The reselector fails the retry rebuild
// fatally, which is the point in the loop where a budget overrun is known.
func TestAgentStreamEndReasonContextBudget(t *testing.T) {
	t.Parallel()

	var invocations atomic.Int32
	provider := &atomicMockProvider{}
	provider.stream = streamScript(&invocations,
		scriptToolCall("budget-call", "lookup"),
		scriptStreamError("", rateLimitedErr()),
	)
	a := New(Deps{})
	a.SetToolProviders(mockToolLoopTools())

	var selectorCalls atomic.Int32
	terminal := streamTerminal(context.Background(), t, a, RunConfig{
		Model:            &sdk.Model{ID: "mock-model", Provider: provider},
		Messages:         []sdk.Message{sdk.UserMessage("original task")},
		SupportsToolCall: true,
		Identity:         SessionContext{BotID: "bot-1"},
		ContextMutations: contextfrag.NewMutationLedger(),
		Retry:            fastRetry,
		ContextStepReselector: func(context.Context, ContextStepSelectionInput) ContextStepSelectionResult {
			if selectorCalls.Add(1) == 1 {
				return ContextStepSelectionResult{}
			}
			return ContextStepSelectionResult{FatalError: contextfrag.ErrBudgetUnsatisfied}
		},
	})
	assertEndReason(t, terminal, EventAgentAbort, event.EndContextBudget)
	if got := int(invocations.Load()); got != 2 {
		t.Fatalf("provider invocations = %d, want 2: the budget stops the run before another call", got)
	}
}

// The text guard aborts the run itself. Its reason is recorded where the guard
// fires, which is before the loop cancels the stream context, so the terminal
// reports the loop and not the cancellation that follows it.
func TestAgentStreamEndReasonLoopDetected(t *testing.T) {
	t.Parallel()

	window := strings.Repeat("the same words over and over ", 10)[:LoopDetectedProbeChars]
	repeated := strings.Repeat(window, LoopDetectedStreakThreshold+1)

	var invocations atomic.Int32
	provider := &atomicMockProvider{}
	provider.stream = streamScript(&invocations, func(ch chan<- sdk.StreamPart) {
		ch <- &sdk.StartPart{}
		ch <- &sdk.StartStepPart{}
		ch <- &sdk.TextStartPart{ID: "mock"}
		ch <- &sdk.TextDeltaPart{ID: "mock", Text: repeated}
	})

	terminal := streamTerminal(context.Background(), t, New(Deps{}), RunConfig{
		Model:            &sdk.Model{ID: "mock-model", Provider: provider},
		Messages:         []sdk.Message{sdk.UserMessage("task")},
		Identity:         SessionContext{BotID: "bot-1"},
		LoopDetection:    LoopDetectionConfig{Enabled: true},
		ContextMutations: contextfrag.NewMutationLedger(),
	})
	assertEndReason(t, terminal, EventAgentAbort, event.EndLoopDetected)
	if got := int(invocations.Load()); got != 1 {
		t.Fatalf("provider invocations = %d, want 1: the guard ends the run it caught", got)
	}
}
