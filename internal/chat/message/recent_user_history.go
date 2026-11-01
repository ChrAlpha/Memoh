package message

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// ListRecentUserHistory projects the newest user rows of a session's history
// to the fields that identify what the user said: ID, role, content, external
// message ID, sender channel identity and platform. The history checkpoint
// does not apply, and metadata, display fields and assets stay in the
// database.
func (s *DBService) ListRecentUserHistory(ctx context.Context, sessionID string, before *HistoryPosition, limit int32) ([]Message, error) {
	pgSessionID, err := dbpkg.ParseUUID(sessionID)
	if err != nil {
		return nil, err
	}
	params := sqlc.ListRecentUserHistoryMessagesParams{
		SessionID: pgSessionID,
		MaxCount:  limit,
	}
	if before != nil {
		params.BeforeTurnPosition = pgtype.Int8{Int64: before.Turn, Valid: true}
		params.BeforeTurnMessageSeq = pgtype.Int8{Int64: before.Seq, Valid: true}
	}
	rows, err := s.queries.ListRecentUserHistoryMessages(ctx, params)
	if err != nil {
		return nil, err
	}
	messages := make([]Message, 0, len(rows))
	for _, row := range rows {
		messages = append(messages, Message{
			ID:                      row.ID.String(),
			SessionID:               sessionID,
			SenderChannelIdentityID: row.SenderChannelIdentityID.String(),
			ExternalMessageID:       dbpkg.TextToString(row.ExternalMessageID),
			Platform:                dbpkg.TextToString(row.Platform),
			Role:                    "user",
			Content:                 row.Content,
		})
	}
	return messages, nil
}
