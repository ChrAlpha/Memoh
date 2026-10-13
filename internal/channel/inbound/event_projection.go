package inbound

import (
	"context"
	"log/slog"

	"github.com/felinics/memoh/internal/chat/timeline"
)

// EventPersister is the narrow slice of the timeline event store this
// package needs to durably stamp an inbound event.
type EventPersister interface {
	PersistEvent(ctx context.Context, botID, sessionID string, event timeline.CanonicalEvent) (string, timeline.CanonicalEvent, error)
}

// persistInboundEvent durably stores the event and reports whether the commit
// succeeded. Callers wake the discuss driver only on success, because the
// driver renders its context from the store.
func persistInboundEvent(
	ctx context.Context,
	store EventPersister,
	log *slog.Logger,
	botID string,
	sessionID string,
	event timeline.CanonicalEvent,
) (string, bool) {
	if store == nil {
		return "", false
	}
	id, _, err := store.PersistEvent(ctx, botID, sessionID, event)
	if err != nil {
		if log != nil {
			log.WarnContext(ctx, "persist inbound event failed", slog.Any("error", err))
		}
		return "", false
	}
	return id, true
}
