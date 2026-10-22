package agenttest

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/internal/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	memprovider "github.com/felinics/memoh/internal/memory/adapters"
)

// The bot and the memory provider MemorySettingsQueries answers for.
const (
	MemoryBotID      = "11111111-1111-1111-1111-111111111111"
	MemoryProviderID = "22222222-2222-2222-2222-222222222222"
)

// MemoryProvider is a memory provider that answers a search with BeforeChat and
// reports what it is handed after a chat on AfterChat.
type MemoryProvider struct {
	memprovider.Provider
	BeforeChat *memprovider.BeforeChatResult
	AfterChat  chan memprovider.AfterChatRequest
}

func (*MemoryProvider) Type() string {
	return "test"
}

func (p *MemoryProvider) OnBeforeChat(context.Context, memprovider.BeforeChatRequest) (*memprovider.BeforeChatResult, error) {
	return p.BeforeChat, nil
}

func (p *MemoryProvider) OnAfterChat(_ context.Context, req memprovider.AfterChatRequest) error {
	p.AfterChat <- req
	return nil
}

// MemorySettingsQueries is a settings store that names MemoryProviderID as
// every bot's memory provider.
type MemorySettingsQueries struct {
	dbstore.Queries
}

func (*MemorySettingsQueries) GetSettingsByBotID(_ context.Context, botID pgtype.UUID) (sqlc.GetSettingsByBotIDRow, error) {
	return sqlc.GetSettingsByBotIDRow{
		BotID:                   botID,
		ReasoningEffort:         "medium",
		CompactionTargetPercent: pgtype.Int4{},
		MemoryProviderID:        UUID(MemoryProviderID),
	}, nil
}

// StoredMessage is a persisted message holding the text as a model message of
// the given role.
func StoredMessage(t *testing.T, id, sessionID, role, text string) messagepkg.Message {
	t.Helper()
	content, err := json.Marshal(runreq.ModelMessage{Role: role, Content: runreq.NewTextContent(text)})
	if err != nil {
		t.Fatalf("marshal stored message: %v", err)
	}
	return messagepkg.Message{ID: id, SessionID: sessionID, Role: role, Content: content}
}
