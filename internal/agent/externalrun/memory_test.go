package externalrun

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	memprovider "github.com/felinics/memoh/internal/memory/adapters"
	"github.com/felinics/memoh/internal/runtimekind"
	"github.com/felinics/memoh/internal/settings"
)

func TestPersistRuntimeRoundSkipsMemoryFormation(t *testing.T) {
	t.Parallel()

	for _, kind := range []runtimekind.Kind{runtimekind.Codex, runtimekind.ClaudeCode, runtimekind.GrokBuild, runtimekind.ACPAgent} {
		for _, eagerUser := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/eager_user=%t", kind, eagerUser), func(t *testing.T) {
				memory := &agenttest.MemoryProvider{AfterChat: make(chan memprovider.AfterChatRequest, 1)}
				registry := memprovider.NewRegistry(slog.New(slog.DiscardHandler))
				registry.Register(agenttest.MemoryProviderID, memory)
				user := agenttest.StoredMessage(t, "message-id", "session-1", "user", "remember this")
				messages := &agenttest.RecordingMessages{}
				if eagerUser {
					messages.Persisted = []messagepkg.PersistInput{{SessionID: user.SessionID, Role: user.Role, Content: user.Content}}
				}
				deps := Deps{Messages: messages, Logger: slog.New(slog.DiscardHandler)}
				deps.Memory = memoryKeeper(deps, settings.NewService(slog.New(slog.DiscardHandler), &agenttest.MemorySettingsQueries{}, nil, nil), registry)
				service := testHost(t, deps)
				req := runreq.ChatRequest{
					BotID: agenttest.MemoryBotID, ThreadID: user.SessionID, Query: "remember this",
					UserMessagePersisted: eagerUser, PersistedUserMessageID: user.ID,
				}
				err := service.persistRound(t.Context(), req, string(kind), "/data", external.PromptResult{
					Output: []sdk.Message{sdk.AssistantMessage("external agent answer")},
				}, externalRoundFailure(nil), true, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				select {
				case got := <-memory.AfterChat:
					if !got.SkipFormation {
						t.Fatal("external runtime turns must not form memory on the platform's models")
					}
					if len(got.Messages) != 2 || got.Messages[0].Content != "remember this" || got.Messages[1].Content != "external agent answer" {
						t.Fatalf("memory transcript = %+v", got.Messages)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("memory provider was not called")
				}
			})
		}
	}
}
