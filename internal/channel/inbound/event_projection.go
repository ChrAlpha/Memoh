package inbound

import (
	"context"
	"log/slog"

	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/chat/timeline"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// EventPersister is the narrow slice of the timeline event store this
// package needs to durably stamp an inbound event.
type EventPersister interface {
	PersistEvent(ctx context.Context, botID, sessionID string, event timeline.CanonicalEvent) (string, timeline.CanonicalEvent, error)
	PersistEventInTx(ctx context.Context, botID, sessionID string, event timeline.CanonicalEvent, withEvent func(queries dbstore.Queries, eventID string) error) (string, bool, error)
}

// PassiveRowWriter writes a passive history row on a caller-owned transaction,
// so the row and its session event commit or roll back together.
type PassiveRowWriter interface {
	PersistWithin(ctx context.Context, queries dbstore.Queries, input messagepkg.PersistInput) (messagepkg.Message, error)
	PublishCreated(message messagepkg.Message)
}

// DiscussWaker is told after a passive event and its row have committed, so
// the discuss side can admit a run for them. Wake must not block the inbound
// path.
type DiscussWaker interface {
	Wake(ctx context.Context, botID, sessionID string)
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
