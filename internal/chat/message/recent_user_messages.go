package message

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// ListRecentUserBySession projects the newest visible user rows of a session
// to the fields that identify what the user said: ID, role, content, external
// message ID, sender channel identity and platform. Compaction does not apply
// and metadata, display fields and assets stay in the database.
func (s *DBService) ListRecentUserBySession(ctx context.Context, sessionID string, beforeMessageID string, limit int32) ([]Message, error) {
	pgSessionID, err := dbpkg.ParseUUID(sessionID)
	if err != nil {
		return nil, err
	}
	var pgBeforeID pgtype.UUID
	if beforeMessageID = strings.TrimSpace(beforeMessageID); beforeMessageID != "" {
		if pgBeforeID, err = dbpkg.ParseUUID(beforeMessageID); err != nil {
			return nil, err
		}
	}
	rows, err := s.queries.ListRecentUserMessagesBySession(ctx, sqlc.ListRecentUserMessagesBySessionParams{
		SessionID:       pgSessionID,
		BeforeMessageID: pgBeforeID,
		MaxCount:        limit,
	})
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
