package pending

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/agentapi"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
)

// SessionQueues is the application surface for user-facing queue operations.
// Steers are transient and live in the configured memory or Redis runtime;
// follow-ups are rows of the session input queue.
type SessionQueues struct {
	SteerSupported bool
	Steer          []sessionruntime.SteerItem
	FollowUp       []sessionruntime.FollowUpItem
}

// QueueInput is one text input an ingress admits into a session queue. It
// carries the identity the ingress authenticated, because the continuation
// that eventually runs the input happens after the request is gone and, in a
// hosted deployment, possibly on another instance: nothing at that point can
// recover which team or user the text belonged to.
type QueueInput struct {
	TeamID       string
	BotID        string
	SessionID    string
	InvocationID string
	// UserID and SourceChannelIdentityID name the sender the way the
	// ingress's ordinary turns do. Empty values are allowed for a channel
	// that has no platform account for the sender.
	UserID                  string
	SourceChannelIdentityID string
	Text                    string
}

// queueTurnCommand rebuilds the ordinary chat turn a queued text would have
// started had the session been idle. Every queue item stores this shape, so
// the continuation replays a command instead of inferring one.
func queueTurnCommand(input QueueInput) (turn.StartTurnCommand, error) {
	teamID, botID, sessionID := strings.TrimSpace(input.TeamID), strings.TrimSpace(input.BotID), strings.TrimSpace(input.SessionID)
	if teamID == "" || botID == "" || sessionID == "" {
		return turn.StartTurnCommand{}, agentapi.ErrQueueInputIncomplete
	}
	text := strings.TrimSpace(input.Text)
	if text == "" || strings.TrimSpace(input.InvocationID) == "" {
		return turn.StartTurnCommand{}, sessionruntime.ErrQueueInvalidReference
	}
	return turn.StartTurnCommand{
		SchemaVersion:           1,
		TeamID:                  teamID,
		Mode:                    turn.ModeChat,
		BotID:                   botID,
		ChatID:                  botID,
		ThreadID:                sessionID,
		UserID:                  strings.TrimSpace(input.UserID),
		SourceChannelIdentityID: strings.TrimSpace(input.SourceChannelIdentityID),
		Query:                   text,
		UserVisibleText:         text,
	}, nil
}

func (in *Inputs) EnqueueSteer(ctx context.Context, input QueueInput) (sessionruntime.SteerItem, error) {
	cmd, err := queueTurnCommand(input)
	if err != nil {
		return sessionruntime.SteerItem{}, err
	}
	payload, err := runreq.EncodeQueueCommand(cmd, nil)
	if err != nil {
		return sessionruntime.SteerItem{}, err
	}
	return in.runtime.EnqueueSteer(ctx, sessionruntime.Key{BotID: cmd.BotID, SessionID: cmd.ThreadID}, uuid.NewString(), strings.TrimSpace(input.InvocationID), payload)
}

func (in *Inputs) ListSessionQueues(ctx context.Context, botID, sessionID string) (SessionQueues, error) {
	key := sessionruntime.Key{BotID: botID, SessionID: sessionID}
	steers, err := in.runtime.PendingSteers(ctx, key, 0)
	if err != nil {
		return SessionQueues{}, err
	}
	followUps, err := in.pendingFollowUps(ctx, key)
	if err != nil {
		return SessionQueues{}, err
	}
	snapshot, err := in.runtime.Snapshot(ctx, botID, sessionID)
	if err != nil {
		return SessionQueues{}, err
	}
	return SessionQueues{Steer: steers, FollowUp: followUps, SteerSupported: sessionruntime.SteerRunAvailable(snapshot.CurrentRunView)}, nil
}

func (in *Inputs) ReorderSteer(ctx context.Context, botID, sessionID string, item, before sessionruntime.SteerPendingRef) ([]sessionruntime.SteerItem, error) {
	return in.runtime.ReorderSteer(ctx, sessionruntime.Key{BotID: botID, SessionID: sessionID}, item, before)
}

// UpdateSteer replaces the text of an accepted steer on behalf of editorUserID,
// who must be the sender the item recorded. Sender, routing, and attachment
// metadata are immutable for a queued command; only the text is rewritten, and
// the backend rechecks accepted status atomically with the edit.
func (in *Inputs) UpdateSteer(ctx context.Context, botID, sessionID, itemID, editorUserID, text string) (sessionruntime.SteerItem, error) {
	key := sessionruntime.Key{BotID: botID, SessionID: sessionID}
	items, err := in.runtime.PendingSteers(ctx, key, 0)
	if err != nil {
		return sessionruntime.SteerItem{}, err
	}
	for _, item := range items {
		if string(item.ID) != itemID {
			continue
		}
		payload, err := runreq.RewriteQueuePayloadText(item.Payload, editorUserID, text)
		if err != nil {
			return sessionruntime.SteerItem{}, err
		}
		return in.runtime.UpdateSteer(ctx, key, item.ID, payload)
	}
	return sessionruntime.SteerItem{}, sessionruntime.ErrQueueNotPending
}

func (in *Inputs) CancelSteer(ctx context.Context, botID, sessionID, itemID string) error {
	return in.runtime.CancelSteer(ctx, sessionruntime.Key{BotID: botID, SessionID: sessionID}, sessionruntime.SteerItemID(itemID))
}
