package agent

import (
	"context"

	"github.com/felinics/memoh/internal/agent/agentapi"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

// The queues of a session and the admission of what waits for it are the
// pending component's.

// StartDiscussSweep starts the sweep that admits the discuss batches nothing
// else did.
func (s *Service) StartDiscussSweep(ctx context.Context) error {
	return s.discussSweep.Start(ctx)
}

// StopDiscussSweep stops the discuss sweep.
func (s *Service) StopDiscussSweep() {
	s.discussSweep.Stop()
}

// StartQueueSweep starts the sweep that starts the follow-ups nothing started.
func (s *Service) StartQueueSweep(ctx context.Context) error {
	return s.queueSweep.Start(ctx)
}

// StopQueueSweep stops the queue sweep.
func (s *Service) StopQueueSweep() {
	s.queueSweep.Stop()
}

// EnqueueFollowUp puts a text the user queued behind the session's run.
func (s *Service) EnqueueFollowUp(ctx context.Context, input agentapi.QueueInput) (sessionruntime.FollowUpItem, error) {
	return s.pending.EnqueueFollowUp(ctx, input)
}

// ReorderFollowUp moves a queued follow-up before another.
func (s *Service) ReorderFollowUp(ctx context.Context, botID, sessionID string, item, before sessionruntime.FollowUpPendingRef) ([]sessionruntime.FollowUpItem, error) {
	return s.pending.ReorderFollowUp(ctx, botID, sessionID, item, before)
}

// UpdateFollowUp replaces the text of a queued follow-up.
func (s *Service) UpdateFollowUp(ctx context.Context, botID, sessionID, itemID, editorUserID, text string) (sessionruntime.FollowUpItem, error) {
	return s.pending.UpdateFollowUp(ctx, botID, sessionID, itemID, editorUserID, text)
}

// CancelFollowUp removes a queued follow-up.
func (s *Service) CancelFollowUp(ctx context.Context, botID, sessionID, itemID string) error {
	return s.pending.CancelFollowUp(ctx, botID, sessionID, itemID)
}

// PromoteFollowUpToSteer hands a queued follow-up to the run in flight as a steer.
func (s *Service) PromoteFollowUpToSteer(ctx context.Context, botID, sessionID string, ref sessionruntime.FollowUpPendingRef) (sessionruntime.PromoteFollowUpResult, error) {
	return s.pending.PromoteFollowUpToSteer(ctx, botID, sessionID, ref)
}

// EnqueueSteer puts a text the user steers into the run in flight.
func (s *Service) EnqueueSteer(ctx context.Context, input agentapi.QueueInput) (sessionruntime.SteerItem, error) {
	return s.pending.EnqueueSteer(ctx, input)
}

// ListSessionQueues lists a session's steers and follow-ups.
func (s *Service) ListSessionQueues(ctx context.Context, botID, sessionID string) (agentapi.SessionQueues, error) {
	return s.pending.ListSessionQueues(ctx, botID, sessionID)
}

// ReorderSteer moves a steer before another.
func (s *Service) ReorderSteer(ctx context.Context, botID, sessionID string, item, before sessionruntime.SteerPendingRef) ([]sessionruntime.SteerItem, error) {
	return s.pending.ReorderSteer(ctx, botID, sessionID, item, before)
}

// UpdateSteer replaces the text of an accepted steer.
func (s *Service) UpdateSteer(ctx context.Context, botID, sessionID, itemID, editorUserID, text string) (sessionruntime.SteerItem, error) {
	return s.pending.UpdateSteer(ctx, botID, sessionID, itemID, editorUserID, text)
}

// CancelSteer removes an accepted steer.
func (s *Service) CancelSteer(ctx context.Context, botID, sessionID, itemID string) error {
	return s.pending.CancelSteer(ctx, botID, sessionID, itemID)
}
