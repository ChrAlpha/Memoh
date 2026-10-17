package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	memprovider "github.com/felinics/memoh/internal/memory/adapters"
	textprune "github.com/felinics/memoh/internal/prune"
	"github.com/felinics/memoh/internal/settings"
)

// What the memory provider is handed is the pruned content the history row
// holds, not the tool result as the model saw it.
func TestMemoryReceivesPrunedPersistedContent(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	memoryProvider := &agenttest.MemoryProvider{AfterChat: make(chan memprovider.AfterChatRequest, 1)}
	registry := memprovider.NewRegistry(slog.New(slog.DiscardHandler))
	registry.Register(agenttest.MemoryProviderID, memoryProvider)
	service := withMemory(wireComponents(&Service{
		messageService:  messages,
		settingsService: settings.NewService(slog.New(slog.DiscardHandler), &agenttest.MemorySettingsQueries{}, nil, nil),
		logger:          slog.New(slog.DiscardHandler),
	}), registry)
	unit := "large tool result "
	huge := strings.Repeat(unit, textprune.DefaultMaxBytes/len(unit)+2)
	err := service.persist.StoreRound(context.Background(), runreq.ChatRequest{
		BotID: agenttest.MemoryBotID, ThreadID: "session-1",
	}, []runreq.ModelMessage{{Role: "tool", Content: runreq.NewTextContent(huge)}}, "", persist.RoundOptions{AllowPendingToolCalls: true})
	if err != nil {
		t.Fatalf("StoreRound() error = %v", err)
	}
	select {
	case got := <-memoryProvider.AfterChat:
		if len(got.Messages) != 1 {
			t.Fatalf("provider messages = %v, want one persisted tool message", got.Messages)
		}
		if got.Messages[0].Content == huge || !strings.Contains(got.Messages[0].Content, textprune.DefaultMarker) {
			t.Fatalf("provider message did not use pruned persisted content: %.120q", got.Messages[0].Content)
		}
	case <-time.After(time.Second):
		t.Fatal("memory provider was not handed the round")
	}
}

type refsRecordingMessageService struct {
	agenttest.RecordingMessages
	persistCount int
}

type partialRefsMessageService struct {
	refsRecordingMessageService
}

func (s *partialRefsMessageService) Persist(ctx context.Context, input messagepkg.PersistInput) (messagepkg.Message, error) {
	if s.persistCount == 1 {
		s.persistCount++
		return messagepkg.Message{}, errors.New("injected second-message failure")
	}
	return s.refsRecordingMessageService.Persist(ctx, input)
}

func (s *refsRecordingMessageService) Persist(ctx context.Context, input messagepkg.PersistInput) (messagepkg.Message, error) {
	msg, err := s.RecordingMessages.Persist(ctx, input)
	if err != nil {
		return msg, err
	}
	s.persistCount++
	msg.ID = fmt.Sprintf("msg-%d", s.persistCount)
	return msg, nil
}

func TestStoreRoundPassesSourceRefsToMemory(t *testing.T) {
	t.Parallel()

	messages := &refsRecordingMessageService{}
	memory := &agenttest.MemoryProvider{AfterChat: make(chan memprovider.AfterChatRequest, 1)}
	registry := memprovider.NewRegistry(slog.New(slog.DiscardHandler))
	registry.Register(agenttest.MemoryProviderID, memory)
	resolver := withMemory(wireComponents(&Service{
		messageService:  messages,
		settingsService: settings.NewService(slog.New(slog.DiscardHandler), &agenttest.MemorySettingsQueries{}, nil, nil),
		logger:          slog.New(slog.DiscardHandler),
	}), registry)

	if err := resolver.persist.StoreRound(context.Background(), runreq.ChatRequest{
		BotID:    agenttest.MemoryBotID,
		ThreadID: "session-1",
		Query:    "hello",
	}, []runreq.ModelMessage{
		{Role: "user", Content: runreq.NewTextContent("hello")},
		{Role: "assistant", Content: runreq.NewTextContent("hi there")},
	}, "model-1", persist.RoundOptions{}); err != nil {
		t.Fatalf("storeRoundWithOptions error: %v", err)
	}

	select {
	case got := <-memory.AfterChat:
		if got.SkipFormation {
			t.Fatal("native model turns must retain memory formation")
		}
		want := []string{"session-1/msg-1", "session-1/msg-2"}
		refs := make([]string, 0, len(got.Messages))
		for _, message := range got.Messages {
			refs = append(refs, message.SourceMessageID)
		}
		if !slices.Equal(refs, want) {
			t.Fatalf("AfterChatRequest message sources = %v, want %v", refs, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("memory provider was not called")
	}
}

func TestStoreRoundSkipsMemoryWhenPersistenceIsPartial(t *testing.T) {
	t.Parallel()

	messages := &partialRefsMessageService{}
	memory := &agenttest.MemoryProvider{AfterChat: make(chan memprovider.AfterChatRequest, 1)}
	registry := memprovider.NewRegistry(slog.New(slog.DiscardHandler))
	registry.Register(agenttest.MemoryProviderID, memory)
	resolver := withMemory(wireComponents(&Service{
		messageService:  messages,
		settingsService: settings.NewService(slog.New(slog.DiscardHandler), &agenttest.MemorySettingsQueries{}, nil, nil),
		logger:          slog.New(slog.DiscardHandler),
	}), registry)

	if err := resolver.persist.StoreRound(context.Background(), runreq.ChatRequest{
		BotID: agenttest.MemoryBotID, ThreadID: "session-1", Query: "hello",
	}, []runreq.ModelMessage{
		{Role: "user", Content: runreq.NewTextContent("hello")},
		{Role: "assistant", Content: runreq.NewTextContent("hi there")},
	}, "model-1", persist.RoundOptions{}); err != nil {
		t.Fatalf("storeRoundWithOptions error: %v", err)
	}

	select {
	case got := <-memory.AfterChat:
		t.Fatalf("memory extraction ran for partial persistence: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
}
