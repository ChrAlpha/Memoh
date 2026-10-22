package timeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// EventStore persists CanonicalEvents to the database.
type EventStore struct {
	queries dbstore.Queries
	logger  *slog.Logger
}

// NewEventStore creates an EventStore.
func NewEventStore(log *slog.Logger, queries dbstore.Queries) *EventStore {
	if log == nil {
		log = slog.Default()
	}
	return &EventStore{
		queries: queries,
		logger:  log.With(slog.String("service", "chat/timeline_event_store")),
	}
}

// PersistEvent writes a CanonicalEvent to the bot_session_events table with a
// freshly allocated monotonic event cursor stamped into its payload. Returns
// the UUID of the persisted event row (empty for ON CONFLICT duplicates) and
// the stamped event the caller must project instead of the input.
func (s *EventStore) PersistEvent(ctx context.Context, botID, sessionID string, event CanonicalEvent) (string, CanonicalEvent, error) {
	pgBotID, err := dbpkg.ParseUUID(botID)
	if err != nil {
		return "", event, fmt.Errorf("invalid bot id: %w", err)
	}
	pgSessionID, err := dbpkg.ParseUUID(sessionID)
	if err != nil {
		return "", event, fmt.Errorf("invalid session id: %w", err)
	}

	original := event
	if cursor, cursorErr := s.queries.NextSessionEventCursor(ctx); cursorErr != nil {
		s.logger.WarnContext(ctx, "allocate session event cursor failed", slog.Any("error", cursorErr))
	} else if stamped, stampErr := assignEventCursor(event, cursor); stampErr != nil {
		s.logger.WarnContext(ctx, "stamp session event cursor failed", slog.Any("error", stampErr))
	} else {
		event = stamped
	}

	eventData, err := json.Marshal(event)
	if err != nil {
		return "", event, fmt.Errorf("marshal event data: %w", err)
	}

	externalMessageID := extractExternalMessageID(event)
	senderID := extractSenderChannelIdentityID(event)
	mentionsMe, repliesToMe := extractAttention(event)

	pgExternalMsgID := pgtype.Text{}
	if externalMessageID != "" {
		pgExternalMsgID = pgtype.Text{String: externalMessageID, Valid: true}
	}

	pgSenderID := pgtype.UUID{}
	if senderID != "" {
		if parsed, parseErr := dbpkg.ParseUUID(senderID); parseErr == nil {
			pgSenderID = parsed
		}
	}

	pgID, err := s.queries.CreateSessionEvent(ctx, sqlc.CreateSessionEventParams{
		BotID:                   pgBotID,
		SessionID:               pgSessionID,
		EventKind:               string(event.Kind()),
		EventData:               eventData,
		ExternalMessageID:       pgExternalMsgID,
		SenderChannelIdentityID: pgSenderID,
		ReceivedAtMs:            event.GetReceivedAtMs(),
		MentionsMe:              mentionsMe,
		RepliesToMe:             repliesToMe,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", original, nil
		}
		return "", event, fmt.Errorf("persist session event: %w", err)
	}

	if pgID.Valid {
		return pgID.String(), event, nil
	}
	return "", original, nil
}

type transactionalQueries interface {
	InTx(ctx context.Context, fn func(dbstore.Queries) error) error
}

// PersistEventInTx inserts the event and, in the same transaction, runs
// withEvent with the transaction-bound queries and the new event id. An event
// that already exists (same external message id) is not inserted and withEvent
// is not called, so inserted is false and the caller writes nothing for it.
func (s *EventStore) PersistEventInTx(ctx context.Context, botID, sessionID string, event CanonicalEvent, withEvent func(queries dbstore.Queries, eventID string) error) (eventID string, inserted bool, err error) {
	txer, ok := s.queries.(transactionalQueries)
	if !ok {
		return "", false, errors.New("timeline event store queries do not support transactions")
	}
	err = txer.InTx(ctx, func(queries dbstore.Queries) error {
		txStore := *s
		txStore.queries = queries
		id, _, persistErr := txStore.PersistEvent(ctx, botID, sessionID, event)
		if persistErr != nil {
			return persistErr
		}
		if id == "" {
			return nil
		}
		eventID, inserted = id, true
		return withEvent(queries, id)
	})
	if err != nil {
		return "", false, err
	}
	return eventID, inserted, nil
}

func extractExternalMessageID(event CanonicalEvent) string {
	switch e := event.(type) {
	case MessageEvent:
		return strings.TrimSpace(e.MessageID)
	case EditEvent:
		return strings.TrimSpace(e.MessageID)
	default:
		return ""
	}
}

// extractAttention reports whether a message event mentions the bot or replies
// to one of its messages. No other kind of event addresses it.
func extractAttention(event CanonicalEvent) (mentionsMe, repliesToMe bool) {
	if message, ok := event.(MessageEvent); ok {
		return message.MentionsMe, message.RepliesToMe
	}
	return false, false
}

func extractSenderChannelIdentityID(event CanonicalEvent) string {
	switch e := event.(type) {
	case MessageEvent:
		if e.Sender != nil {
			return strings.TrimSpace(e.Sender.ID)
		}
	case EditEvent:
		if e.Sender != nil {
			return strings.TrimSpace(e.Sender.ID)
		}
	case ServiceEvent:
		if e.Actor != nil {
			return strings.TrimSpace(e.Actor.ID)
		}
	}
	return ""
}
