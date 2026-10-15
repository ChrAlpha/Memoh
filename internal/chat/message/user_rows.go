package message

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// UserRow is a user row to write under an identity the caller chose.
type UserRow struct {
	// ID is the row's message id. A row of the session that already has it is
	// kept as it is and not written again, so a caller that writes the same
	// rows after a failure, or from two processes, ends with one of each.
	ID string
	// Input is the row. It is a user row, and names no turn: each row opens a
	// turn of its own, the way an inbound message that nobody answers yet does.
	Input PersistInput
}

// UserRowPersister writes a batch of user rows atomically.
type UserRowPersister interface {
	PersistUserRows(ctx context.Context, rows []UserRow) ([]Message, error)
}

// PersistUserRows writes the rows in the order given, one turn each, so their
// positions follow that order, in one transaction. It returns the rows that
// were written; rows that existed are left out. Created events are published
// once the transaction has committed.
func (s *DBService) PersistUserRows(ctx context.Context, rows []UserRow) ([]Message, error) {
	if s == nil || s.queries == nil {
		return nil, errors.New("message service is not configured")
	}
	if len(rows) == 0 {
		return nil, nil
	}
	botID := strings.TrimSpace(rows[0].Input.BotID)
	sessionID := strings.TrimSpace(rows[0].Input.SessionID)
	for _, row := range rows {
		input := row.Input
		switch {
		case !strings.EqualFold(strings.TrimSpace(input.Role), "user"):
			return nil, errors.New("user rows are user messages")
		case strings.TrimSpace(input.BotID) != botID || strings.TrimSpace(input.SessionID) != sessionID:
			return nil, errors.New("user rows span more than one session")
		case input.SkipHistoryTurn || input.TurnID != "" || input.TurnPosition != nil:
			return nil, errors.New("user rows open a turn of their own")
		}
	}
	txer, ok := s.queries.(transactionalQueries)
	if !ok {
		return nil, errors.New("persistence store does not support user row batches")
	}
	var written []Message
	err := txer.InTx(ctx, func(queries dbstore.Queries) error {
		written = written[:0]
		txService := *s
		txService.queries = queries
		txService.publisher = nil
		for _, row := range rows {
			message, created, err := txService.persistUserRow(ctx, row)
			if err != nil {
				return err
			}
			if created {
				written = append(written, message)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, message := range written {
		s.publishMessageCreated(message)
	}
	return written, nil
}

func (s *DBService) persistUserRow(ctx context.Context, row UserRow) (Message, bool, error) {
	direct, ok := s.queries.(directHistoryTurnWriter)
	if !ok {
		return Message{}, false, errors.New("persistence store does not support direct history turn writes")
	}
	messageID, err := dbpkg.ParseUUID(strings.TrimSpace(row.ID))
	if err != nil {
		return Message{}, false, fmt.Errorf("invalid user row id: %w", err)
	}
	prepared, err := s.preparePersistMessage(ctx, row.Input)
	if err != nil {
		return Message{}, false, err
	}
	if !prepared.sessionID.Valid {
		return Message{}, false, errors.New("user rows need a session")
	}
	if _, err := s.queries.GetMessageByIDBySession(ctx, sqlc.GetMessageByIDBySessionParams{
		SessionID: prepared.sessionID, MessageID: messageID, IncludeUnadmitted: true,
	}); err == nil {
		return Message{}, false, nil
	} else if !errors.Is(err, pgx.ErrNoRows) && !errors.Is(err, dbpkg.ErrNotFound) {
		return Message{}, false, err
	}
	createArg := prepared.createArg
	created, err := direct.CreateMessageWithHistoryTurn(ctx, sqlc.CreateMessageWithHistoryTurnParams{
		MessageID:               messageID,
		BotID:                   createArg.BotID,
		SessionID:               createArg.SessionID,
		SenderChannelIdentityID: createArg.SenderChannelIdentityID,
		SenderUserID:            createArg.SenderUserID,
		ExternalMessageID:       createArg.ExternalMessageID,
		SourceReplyToMessageID:  createArg.SourceReplyToMessageID,
		Role:                    createArg.Role,
		Content:                 createArg.Content,
		Metadata:                createArg.Metadata,
		Usage:                   createArg.Usage,
		SessionMode:             createArg.SessionMode,
		RuntimeType:             createArg.RuntimeType,
		ModelID:                 createArg.ModelID,
		EventID:                 createArg.EventID,
		DisplayText:             createArg.DisplayText,
		RunID:                   createArg.RunID,
		TurnID:                  newPGUUID(),
		TurnMessageSeq:          pgtype.Int8{Int64: 1, Valid: true},
	})
	if err != nil {
		return Message{}, false, err
	}
	message, err := s.finishPersistedMessage(ctx, toMessageFromCreateWithHistoryTurn(created, createArg, prepared.metadata), messageID, row.Input.Assets)
	if err != nil {
		return Message{}, false, err
	}
	return message, true, nil
}
