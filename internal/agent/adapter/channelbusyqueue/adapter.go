// Package channelbusyqueue adapts the session input queue to the channel
// boundary's view of it, so the channel process parks and starts the platform
// messages that met a busy session without importing the session runtime.
package channelbusyqueue

import (
	"context"
	"errors"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/channel/inbound"
)

// Adapter is inbound.BusyQueue over the PostgreSQL session input queue.
type Adapter struct{ queue *sessionruntime.QueuedInputs }

func New(queue *sessionruntime.QueuedInputs) *Adapter { return &Adapter{queue: queue} }

var _ inbound.BusyQueue = (*Adapter)(nil)

func (a *Adapter) Enqueue(ctx context.Context, in inbound.BusyInput) error {
	_, err := a.queue.Enqueue(ctx, sessionruntime.QueueEnqueue{
		Key:          sessionruntime.Key{BotID: in.BotID, SessionID: in.SessionID},
		Origin:       sessionruntime.QueueOrigin(in.Origin),
		InvocationID: in.InvocationID,
		MergeKey:     in.MergeKey,
		Payload:      in.Payload,
	})
	switch {
	case errors.Is(err, sessionruntime.ErrQueueCapacityExceeded):
		return inbound.ErrBusyQueueFull
	case errors.Is(err, sessionruntime.ErrQueueInvocationConflict):
		return inbound.ErrBusyQueueConflict
	default:
		return err
	}
}

func (a *Adapter) ClaimBatch(ctx context.Context, botID, sessionID string) (inbound.BusyBatch, bool, error) {
	batch, ok, err := a.queue.ClaimBatch(ctx, sessionruntime.Key{BotID: botID, SessionID: sessionID}, sessionruntime.QueueOriginPlatformBusy)
	if err != nil || !ok {
		return inbound.BusyBatch{}, false, err
	}
	inputs := make([]inbound.BusyBatchInput, len(batch.Items))
	for i, item := range batch.Items {
		inputs[i] = inbound.BusyBatchInput{ID: item.ID, Payload: item.Payload}
	}
	return inbound.BusyBatch{
		ID: batch.ID, ClaimToken: batch.ClaimToken, BotID: botID, SessionID: sessionID,
		Recovered: batch.Recovered, Inputs: inputs,
	}, true, nil
}

func (a *Adapter) SettleBatch(ctx context.Context, batch inbound.BusyBatch) error {
	return a.queue.SettleBatch(ctx, queueBatch(batch))
}

func (a *Adapter) ReleaseBatch(ctx context.Context, batch inbound.BusyBatch) error {
	return a.queue.ReleaseBatch(ctx, queueBatch(batch))
}

func (a *Adapter) Reject(ctx context.Context, inputID string) error {
	return a.queue.Reject(ctx, inputID, sessionruntime.QueueErrorFollowUpCommandInvalid)
}

func (a *Adapter) Drainable(ctx context.Context) ([]inbound.BusySession, error) {
	keys, err := a.queue.Drainable(ctx, sessionruntime.QueueOriginPlatformBusy)
	if err != nil {
		return nil, err
	}
	sessions := make([]inbound.BusySession, len(keys))
	for i, key := range keys {
		sessions[i] = inbound.BusySession{BotID: key.BotID, SessionID: key.SessionID}
	}
	return sessions, nil
}

func queueBatch(batch inbound.BusyBatch) sessionruntime.QueueBatch {
	return sessionruntime.QueueBatch{
		ID: batch.ID, ClaimToken: batch.ClaimToken,
		Key:       sessionruntime.Key{BotID: batch.BotID, SessionID: batch.SessionID},
		Origin:    sessionruntime.QueueOriginPlatformBusy,
		Recovered: batch.Recovered,
	}
}
