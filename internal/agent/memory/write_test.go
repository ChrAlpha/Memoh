package memory

import (
	"context"
	"log/slog"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	memprovider "github.com/felinics/memoh/internal/memory/adapters"
	"github.com/felinics/memoh/internal/memory/storefs"
)

func TestToProviderMessagesKeepsPersistedContentAndSourceAligned(t *testing.T) {
	t.Parallel()

	persisted := []messagepkg.Message{
		agenttest.StoredMessage(t, "msg-a", "sess-1", "assistant", "first stored answer"),
		agenttest.StoredMessage(t, "msg-b", "sess-1", "assistant", "second stored answer"),
	}
	got := providerMessages(persisted)
	if len(got) != 2 {
		t.Fatalf("provider messages = %v, want two persisted messages", got)
	}
	wantRefs := []string{"sess-1/msg-a", "sess-1/msg-b"}
	for i, want := range wantRefs {
		if got[i].SourceMessageID != want {
			t.Fatalf("provider message %d source = %q, want %q", i, got[i].SourceMessageID, want)
		}
	}
	if got[0].Content != "first stored answer" || got[1].Content != "second stored answer" {
		t.Fatalf("provider message content is misaligned: %v", got)
	}
}

func TestRoundPersistedUsesPersistedUserContent(t *testing.T) {
	t.Parallel()

	user := agenttest.StoredMessage(t, "msg-user", "session-1", "user", "persisted user question")
	messages := &agenttest.RecordingMessages{Persisted: []messagepkg.PersistInput{{
		SessionID: user.SessionID, Role: user.Role, Content: user.Content,
	}}}
	memory := &agenttest.MemoryProvider{AfterChat: make(chan memprovider.AfterChatRequest, 1)}
	registry := memprovider.NewRegistry(slog.New(slog.DiscardHandler))
	registry.Register(agenttest.MemoryProviderID, memory)
	keeper := newTestKeeper(t, func(d *Deps) {
		d.Registry = registry
		d.Messages = messages
	})

	keeper.RoundPersisted(context.Background(), runreq.ChatRequest{
		BotID: agenttest.MemoryBotID, ThreadID: "session-1",
		Query:                "runtime query with generated headers",
		UserMessagePersisted: true, PersistedUserMessageID: "msg-user",
	}, []messagepkg.Message{
		agenttest.StoredMessage(t, "msg-assistant", "session-1", "assistant", "stored answer"),
	})

	got := <-memory.AfterChat
	if len(got.Messages) != 2 {
		t.Fatalf("memory messages = %v, want persisted user and assistant", got.Messages)
	}
	if got.Messages[0].Content != "persisted user question" || got.Messages[0].SourceMessageID != "session-1/msg-user" {
		t.Fatalf("persisted user message = %#v", got.Messages[0])
	}
	if got.Messages[1].Content != "stored answer" || got.Messages[1].SourceMessageID != "session-1/msg-assistant" {
		t.Fatalf("persisted assistant message = %#v", got.Messages[1])
	}
}

type conversationWriteProvider struct {
	memprovider.Provider
	afterChat chan context.Context
}

func (*conversationWriteProvider) Type() string { return "test" }

func (p *conversationWriteProvider) OnAfterChat(ctx context.Context, _ memprovider.AfterChatRequest) error {
	p.afterChat <- ctx
	return nil
}

func TestWriteMarksPostTurnWritesAsConversationWrites(t *testing.T) {
	t.Parallel()

	provider := &conversationWriteProvider{afterChat: make(chan context.Context, 1)}
	registry := memprovider.NewRegistry(slog.New(slog.DiscardHandler))
	registry.Register(agenttest.MemoryProviderID, provider)
	keeper := newTestKeeper(t, func(d *Deps) { d.Registry = registry })

	err := keeper.write(context.Background(), runreq.ChatRequest{BotID: agenttest.MemoryBotID, ThreadID: "session-1"}, agenttest.MemoryBotID, []messagepkg.Message{
		agenttest.StoredMessage(t, "msg-assistant", "session-1", "assistant", "stored answer"),
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if ctx := <-provider.afterChat; !storefs.IsConversationWrite(ctx) {
		t.Fatal("post-turn memory write is not marked as a conversation write")
	}
}
