package application

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	messagepkg "github.com/memohai/memoh/internal/chat/message"
	memprovider "github.com/memohai/memoh/internal/memory/adapters"
	"github.com/memohai/memoh/internal/settings"
)

func TestRoundSourceRefs(t *testing.T) {
	t.Parallel()

	persisted := []messagepkg.Message{
		{ID: "msg-a", SessionID: "sess-1"},
		{ID: "msg-b"},
		{ID: ""},
	}

	got := roundSourceRefs(ChatRequest{
		ThreadID:               "sess-1",
		UserMessagePersisted:   true,
		PersistedUserMessageID: "msg-user",
	}, persisted)
	want := []string{"sess-1/msg-user", "sess-1/msg-a", "sess-1/msg-b"}
	if !slices.Equal(got, want) {
		t.Fatalf("roundSourceRefs = %v, want %v", got, want)
	}

	got = roundSourceRefs(ChatRequest{ThreadID: "sess-1"}, persisted[:1])
	want = []string{"sess-1/msg-a"}
	if !slices.Equal(got, want) {
		t.Fatalf("roundSourceRefs without persisted user message = %v, want %v", got, want)
	}

	if got := roundSourceRefs(ChatRequest{ThreadID: "sess-1"}, nil); len(got) != 0 {
		t.Fatalf("roundSourceRefs with no messages = %v, want empty", got)
	}
}

type refsRecordingMessageService struct {
	recordingMessageService
	persistCount int
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
		if !slices.Equal(got.SourceMessageIDs, want) {
			t.Fatalf("AfterChatRequest.SourceMessageIDs = %v, want %v", got.SourceMessageIDs, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("memory provider was not called")
	}
}
