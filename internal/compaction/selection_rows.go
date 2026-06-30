package compaction

import (
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/db"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
	"github.com/memohai/memoh/internal/historyfrag"
	messagepkg "github.com/memohai/memoh/internal/message"
)

func recordCandidatesFromRows(rows []sqlc.ListUncompactedMessagesBySessionRow, viewHeadTurnID string) ([]RecordCompactionCandidate, int) {
	records := make([]historyfrag.HistoryRecord, 0, len(rows))
	skipped := 0
	for _, row := range rows {
		record, err := historyfrag.FromDBMessage(rowToMessage(row), rowScopeFallback(row, viewHeadTurnID))
		if err != nil {
			skipped++
			continue
		}
		records = append(records, record)
	}
	return recordCandidatesFromRecords(records), skipped
}

func rowToMessage(row sqlc.ListUncompactedMessagesBySessionRow) messagepkg.Message {
	return messagepkg.Message{
		ID:                      formatUUID(row.ID),
		BotID:                   formatUUID(row.BotID),
		SessionID:               formatUUID(row.SessionID),
		TurnID:                  formatUUID(row.TurnID),
		TurnMessageSeq:          int8Value(row.TurnMessageSeq),
		SenderChannelIdentityID: formatUUID(row.SenderChannelIdentityID),
		SenderUserID:            formatUUID(row.SenderUserID),
		SenderDisplayName:       textValue(row.SenderDisplayName),
		SenderAvatarURL:         textValue(row.SenderAvatarUrl),
		Platform:                textValue(row.Platform),
		ExternalMessageID:       textValue(row.ExternalMessageID),
		SourceReplyToMessageID:  textValue(row.SourceReplyToMessageID),
		Role:                    row.Role,
		Content:                 row.Content,
		Metadata:                metadataMap(row.Metadata),
		Usage:                   row.Usage,
		SessionMode:             strings.TrimSpace(row.SessionMode),
		RuntimeType:             strings.TrimSpace(row.RuntimeType),
		CompactID:               formatUUID(row.CompactID),
		EventID:                 formatUUID(row.EventID),
		DisplayContent:          textValue(row.DisplayText),
		CreatedAt:               row.CreatedAt.Time,
	}
}

func rowScopeFallback(row sqlc.ListUncompactedMessagesBySessionRow, viewHeadTurnID string) historyfrag.ScopeFallback {
	if rowViewHeadID := formatUUID(row.ViewHeadTurnID); rowViewHeadID != "" {
		viewHeadTurnID = rowViewHeadID
	}
	return historyfrag.ScopeFallback{
		ViewHeadTurnID:   strings.TrimSpace(viewHeadTurnID),
		ConversationType: textValue(row.ConversationType),
		ConversationName: strings.TrimSpace(row.ConversationName),
		ReplyTarget:      textValue(row.ReplyTarget),
	}
}

func textValue(value pgtype.Text) string {
	if !value.Valid {
		return ""
	}
	return strings.TrimSpace(value.String)
}

func int8Value(value pgtype.Int8) int64 {
	if !value.Valid {
		return 0
	}
	return value.Int64
}

func metadataMap(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

func messageIDsFromRecordRefs(refs []contextfrag.ContextRef) ([]pgtype.UUID, error) {
	ids := make([]pgtype.UUID, 0, len(refs))
	for _, ref := range refs {
		id, err := db.ParseUUID(ref.ID)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}
