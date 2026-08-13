package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	messagepkg "github.com/memohai/memoh/internal/chat/message"
	memprovider "github.com/memohai/memoh/internal/memory/adapters"
	"github.com/memohai/memoh/internal/settings"
)

func TestToProviderMessagesKeepsContentAndPersistedSourceAligned(t *testing.T) {
	t.Parallel()

	persisted := []messagepkg.Message{
		{ID: "msg-a", SessionID: "sess-1"},
		{ID: "msg-b"},
	}
	got := toProviderMessages(ChatRequest{
		ThreadID:               "sess-1",
		Query:                  "original user question",
		UserMessagePersisted:   true,
		PersistedUserMessageID: "msg-user",
	}, []ModelMessage{
		{Role: "assistant", Content: newTextContent("first answer")},
		{Role: "assistant", Content: newTextContent("second answer")},
	}, persisted)
	if len(got) != 3 {
		t.Fatalf("provider messages = %v, want reused user plus two persisted messages", got)
	}
	wantRefs := []string{"sess-1/msg-user", "sess-1/msg-a", "sess-1/msg-b"}
	for i, want := range wantRefs {
		if got[i].SourceMessageID != want {
			t.Fatalf("provider message %d source = %q, want %q", i, got[i].SourceMessageID, want)
		}
	}
	if got[0].Content != "original user question" || got[1].Content != "first answer" || got[2].Content != "second answer" {
		t.Fatalf("provider message content is misaligned: %v", got)
	}
}

type refsRecordingMessageService struct {
	recordingMessageService
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
	msg, err := s.recordingMessageService.Persist(ctx, input)
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
	memory := &storeRoundMemoryProvider{afterChat: make(chan memprovider.AfterChatRequest, 1)}
	registry := memprovider.NewRegistry(slog.New(slog.DiscardHandler))
	registry.Register(storeRoundMemoryProviderID, memory)
	resolver := &Service{
		messageService:  messages,
		memoryRegistry:  registry,
		settingsService: settings.NewService(slog.New(slog.DiscardHandler), &storeRoundSettingsQueries{}, nil, nil),
		logger:          slog.New(slog.DiscardHandler),
	}

	if err := resolver.storeRound(context.Background(), ChatRequest{
		BotID:    storeRoundBotID,
		ThreadID: "session-1",
		Query:    "hello",
	}, []ModelMessage{
		{Role: "user", Content: newTextContent("hello")},
		{Role: "assistant", Content: newTextContent("hi there")},
	}, "model-1"); err != nil {
		t.Fatalf("storeRound error: %v", err)
	}

	select {
	case got := <-memory.afterChat:
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
	memory := &storeRoundMemoryProvider{afterChat: make(chan memprovider.AfterChatRequest, 1)}
	registry := memprovider.NewRegistry(slog.New(slog.DiscardHandler))
	registry.Register(storeRoundMemoryProviderID, memory)
	resolver := &Service{
		messageService:  messages,
		memoryRegistry:  registry,
		settingsService: settings.NewService(slog.New(slog.DiscardHandler), &storeRoundSettingsQueries{}, nil, nil),
		logger:          slog.New(slog.DiscardHandler),
	}

	if err := resolver.storeRound(context.Background(), ChatRequest{
		BotID: storeRoundBotID, ThreadID: "session-1", Query: "hello",
	}, []ModelMessage{
		{Role: "user", Content: newTextContent("hello")},
		{Role: "assistant", Content: newTextContent("hi there")},
	}, "model-1"); err != nil {
		t.Fatalf("storeRound error: %v", err)
	}

	select {
	case got := <-memory.afterChat:
		t.Fatalf("memory extraction ran for partial persistence: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
}
