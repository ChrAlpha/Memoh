package pending

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/felinics/memoh/internal/agent/agentapi"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
)

// A deployment with no database for the queue has nothing queued: a new turn is
// never held behind queued input, nothing is claimable, and every operation
// that would use the queue says it is unavailable.
func TestWithoutAQueueNothingWaitsAndQueueOperationsAreUnavailable(t *testing.T) {
	inputs := New(Deps{Logger: slog.New(slog.DiscardHandler), Queries: noDiscuss{}, Continuations: noContinuations{}})
	ctx := t.Context()
	key := sessionruntime.Key{BotID: "bot-1", SessionID: "session-1"}

	require.False(t, inputs.Queueing())
	require.NoError(t, inputs.GateTurn(ctx, turn.StartTurnCommand{BotID: "bot-1", ThreadID: "session-1", IdempotencyKey: "m1"}))
	gate, err := inputs.Gate(ctx, key, "m1")
	require.NoError(t, err)
	require.Equal(t, sessionruntime.QueueGate{}, gate)

	started, deferred, err := inputs.drainFollowUps(ctx, key)
	require.NoError(t, err)
	require.False(t, started)
	require.False(t, deferred)

	_, err = inputs.EnqueueFollowUp(ctx, agentapi.QueueInput{TeamID: "team-1", BotID: "bot-1", SessionID: "session-1", InvocationID: "m1", Text: "hello"})
	require.ErrorIs(t, err, sessionruntime.ErrLiveQueueUnavailable)
	_, err = inputs.ReorderFollowUp(ctx, "bot-1", "session-1", sessionruntime.FollowUpPendingRef{}, sessionruntime.FollowUpPendingRef{})
	require.ErrorIs(t, err, sessionruntime.ErrLiveQueueUnavailable)
	require.ErrorIs(t, inputs.CancelFollowUp(ctx, "bot-1", "session-1", "item-1"), sessionruntime.ErrLiveQueueUnavailable)
	_, err = inputs.PromoteFollowUpToSteer(ctx, "bot-1", "session-1", sessionruntime.FollowUpPendingRef{})
	require.ErrorIs(t, err, sessionruntime.ErrLiveQueueUnavailable)

	require.Error(t, NewQueueSweep(inputs).Start(ctx), "a sweep of a queue that is not there is not started")
}

// With a queue the same gate is the queue's own verdict.
func TestWithAQueueTheGateIsTheQueuesVerdict(t *testing.T) {
	queue := &gated{gate: sessionruntime.QueueGate{Waiting: true}}
	inputs := New(Deps{Logger: slog.New(slog.DiscardHandler), Queries: noDiscuss{}, Queue: queue, Continuations: noContinuations{}})

	require.True(t, inputs.Queueing())
	require.ErrorIs(t, inputs.GateTurn(t.Context(), turn.StartTurnCommand{BotID: "bot-1", ThreadID: "session-1", IdempotencyKey: "late"}), turn.ErrSessionBusy)

	queue.gate = sessionruntime.QueueGate{Seen: true}
	require.ErrorIs(t, inputs.GateTurn(t.Context(), turn.StartTurnCommand{BotID: "bot-1", ThreadID: "session-1", IdempotencyKey: "again"}), turn.ErrDuplicateTurn)

	// The queue's own admission is the one command that neither waits nor is counted.
	require.NoError(t, inputs.GateTurn(t.Context(), turn.StartTurnCommand{BotID: "bot-1", ThreadID: "session-1", IdempotencyKey: "queued", Origin: turn.OriginPending}))
}

type gated struct {
	Store
	gate sessionruntime.QueueGate
}

func (g *gated) Gate(context.Context, sessionruntime.Key, string) (sessionruntime.QueueGate, error) {
	return g.gate, nil
}
