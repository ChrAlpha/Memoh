package persist

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/botenv"
	"github.com/felinics/memoh/internal/agent/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// newTestWriter is a Writer over d, with a discarding logger and a bot
// environment that has no settings unless d names one.
func newTestWriter(d Deps) *Writer {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Bot == nil {
		d.Bot = botenv.New(botenv.Deps{Logger: d.Logger})
	}
	return New(d)
}

const storeRoundBotID = "11111111-1111-1111-1111-111111111111"

func int64Pointer(value int64) *int64 { return &value }

// storeRoundSettingsQueries answers the bot settings read a round makes.
type storeRoundSettingsQueries struct {
	dbstore.Queries
}

func (*storeRoundSettingsQueries) GetSettingsByBotID(_ context.Context, botID pgtype.UUID) (sqlc.GetSettingsByBotIDRow, error) {
	return sqlc.GetSettingsByBotIDRow{
		BotID:                   botID,
		ReasoningEffort:         "medium",
		CompactionTargetPercent: pgtype.Int4{},
	}, nil
}

// recordedMessages is what a message service that stores inputs verbatim
// returns for them.
func recordedMessages(inputs []messagepkg.PersistInput) []messagepkg.Message {
	messages := make([]messagepkg.Message, 0, len(inputs))
	for _, input := range inputs {
		messages = append(messages, messagepkg.Message{
			BotID:          input.BotID,
			SessionID:      input.SessionID,
			Role:           input.Role,
			Content:        input.Content,
			DisplayContent: input.DisplayText,
			Metadata:       input.Metadata,
			TurnID:         runreq.FirstNonEmpty(input.TurnID, "test-turn"),
		})
	}
	return messages
}
