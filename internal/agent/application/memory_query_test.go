package application

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

type memoryQueryMessageService struct {
	recordingMessageService
	messages []messagepkg.Message

	historyReads    int
	recentUserCalls []memoryQueryRecentUserCall
}

type memoryQueryRecentUserCall struct {
	sessionID       string
	beforeMessageID string
	limit           int32
}

// ListRecentUserBySession follows the SQL contract over messages in history
// order: user rows only, before the cutoff row, the newest limit oldest first.
func (s *memoryQueryMessageService) ListRecentUserBySession(_ context.Context, sessionID, beforeMessageID string, limit int32) ([]messagepkg.Message, error) {
	s.historyReads++
	s.recentUserCalls = append(s.recentUserCalls, memoryQueryRecentUserCall{sessionID: sessionID, beforeMessageID: beforeMessageID, limit: limit})
	rows := s.messages
	if beforeMessageID != "" {
		cutoff := slices.IndexFunc(rows, func(m messagepkg.Message) bool { return m.ID == beforeMessageID })
		if cutoff < 0 {
			return nil, nil
		}
		rows = rows[:cutoff]
	}
	var users []messagepkg.Message
	for _, row := range rows {
		if row.Role == "user" {
			users = append(users, row)
		}
	}
	return users[max(0, len(users)-int(limit)):], nil
}

func (s *memoryQueryMessageService) ListActiveSinceBySession(context.Context, string, time.Time) ([]messagepkg.Message, error) {
	s.historyReads++
	return append([]messagepkg.Message(nil), s.messages...), nil
}

func (s *memoryQueryMessageService) ListActiveSinceBySessionWithinBytes(context.Context, string, time.Time, int64, bool) ([]messagepkg.Message, error) {
	s.historyReads++
	return append([]messagepkg.Message(nil), s.messages...), nil
}

func (s *memoryQueryMessageService) ListTurnResponseSourcesSinceBySessionWithinBytes(context.Context, string, time.Time, int64) ([]messagepkg.Message, error) {
	return append([]messagepkg.Message(nil), s.messages...), nil
}

func (*memoryQueryMessageService) MeasureActiveBySession(context.Context, string, time.Time) (messagepkg.ActiveMessagesMeasure, error) {
	return messagepkg.ActiveMessagesMeasure{}, nil
}

func TestMemoryQueryBuilderCombinesRecentUserMessages(t *testing.T) {
	t.Parallel()

	builder := memoryQueryBuilder{MaxRecentMessages: 2, MaxBytes: 2000, MaxLines: 20, MaxMessageRunes: 200}
	query := builder.Build(ChatRequest{Query: "What should I pack?"}, []historyfrag.HistoryRecord{
		{ModelMessage: ModelMessage{Role: "user", Content: newTextContent("I am visiting Berlin next week")}},
		{ModelMessage: ModelMessage{Role: "assistant", Content: newTextContent("Sounds fun.")}},
		{ModelMessage: ModelMessage{Role: "user", Content: newTextContent("I prefer tea over coffee")}},
		{ModelMessage: ModelMessage{Role: "user", Content: newTextContent("What should I pack?")}},
	})

	if query.Source != "current_query_with_history" {
		t.Fatalf("Source = %q, want current_query_with_history", query.Source)
	}
	if query.RecentMessages != 2 {
		t.Fatalf("RecentMessages = %d, want 2", query.RecentMessages)
	}
	for _, want := range []string{"Current user request:", "What should I pack?", "I am visiting Berlin", "I prefer tea"} {
		if !strings.Contains(query.Query, want) {
			t.Fatalf("query missing %q: %s", want, query.Query)
		}
	}
	if strings.Count(query.Query, "What should I pack?") != 1 {
		t.Fatalf("expected current query to be deduplicated, got: %s", query.Query)
	}
}

func TestMemoryQueryBuilderTruncates(t *testing.T) {
	t.Parallel()

	builder := memoryQueryBuilder{MaxRecentMessages: 1, MaxBytes: 120, MaxLines: 6, MaxMessageRunes: 500}
	query := builder.Build(ChatRequest{Query: strings.Repeat("current ", 30)}, []historyfrag.HistoryRecord{
		{ModelMessage: ModelMessage{Role: "user", Content: newTextContent(strings.Repeat("history ", 30))}},
	})

	if !query.Truncated {
		t.Fatal("expected query to be marked truncated")
	}
	if len(query.Query) > 120 {
		t.Fatalf("query length = %d, want <= 120: %q", len(query.Query), query.Query)
	}
}

func TestMemoryQueryBuilderSkipsEmptyVisibleQuery(t *testing.T) {
	t.Parallel()

	query := defaultMemoryQueryBuilder().Build(ChatRequest{Query: "   "}, []historyfrag.HistoryRecord{
		{ModelMessage: ModelMessage{Role: "user", Content: newTextContent("history")}},
	})
	if query.Query != "" {
		t.Fatalf("expected empty query, got %+v", query)
	}
}

func TestBuildMemoryQueryReadsRecentUserMessagesBeforeRetryCutoff(t *testing.T) {
	t.Parallel()

	botID := "00000000-0000-0000-0000-00000000b715"
	sessionID := "00000000-0000-0000-0000-00000000e715"
	compactID := "00000000-0000-0000-0000-00000000c715"
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	prior := persistedHistoryMessage(t, "prior-user", "user", "prior visible context")
	prior.CompactID = compactID
	prior.CreatedAt = base.Add(10 * time.Minute)
	reply := persistedHistoryMessage(t, "assistant-reply", "assistant", "assistant reply")
	reply.CreatedAt = base.Add(15 * time.Minute)
	cutoff := persistedHistoryMessage(t, "cutoff-user", "user", "edited-away request")
	cutoff.CreatedAt = base.Add(20 * time.Minute)

	messages := &memoryQueryMessageService{messages: []messagepkg.Message{prior, reply, cutoff}}
	resolver := &Service{
		messageService: messages,
		queries: &recordingCompactionLogQueries{logs: []sqlc.BotHistoryMessageCompact{{
			ID:            mustPGUUID(t, compactID),
			BotID:         mustPGUUID(t, botID),
			SessionID:     mustPGUUID(t, sessionID),
			Status:        "ok",
			Summary:       "compacted summary text",
			AnchorStartMs: prior.CreatedAt.UnixMilli(),
			AnchorEndMs:   prior.CreatedAt.UnixMilli(),
		}}},
	}
	query := resolver.buildMemoryQuery(context.Background(), ChatRequest{
		BotID:                        botID,
		ThreadID:                     sessionID,
		Query:                        "replacement request",
		HistoryCutoffBeforeMessageID: cutoff.ID,
	})

	want := memoryQueryRecentUserCall{sessionID: sessionID, beforeMessageID: cutoff.ID, limit: defaultMemoryQueryUserRowScan}
	if len(messages.recentUserCalls) != 1 || messages.recentUserCalls[0] != want {
		t.Fatalf("recent user reads = %+v, want one %+v", messages.recentUserCalls, want)
	}
	if !strings.Contains(query.Query, "prior visible context") {
		t.Fatalf("memory query lost the user's own compacted message: %q", query.Query)
	}
	for _, excluded := range []string{"edited-away request", "assistant reply", "compacted summary text"} {
		if strings.Contains(query.Query, excluded) {
			t.Fatalf("memory query carried %q: %q", excluded, query.Query)
		}
	}
}

func TestBuildMemoryQuerySkipsPersistedCurrentUserMessage(t *testing.T) {
	t.Parallel()

	earlier := persistedHistoryMessage(t, "earlier-user", "user", "earlier request")
	current := persistedHistoryMessage(t, "current-user", "user", "[header] current request")
	current.ExternalMessageID = "external-1"
	current.Platform = "telegram"
	resolver := &Service{messageService: &memoryQueryMessageService{messages: []messagepkg.Message{earlier, current}}}

	query := resolver.buildMemoryQuery(context.Background(), ChatRequest{
		ThreadID:             "00000000-0000-0000-0000-00000000e716",
		Query:                "current request",
		UserMessagePersisted: true,
		ExternalMessageID:    "external-1",
		CurrentChannel:       "telegram",
	})

	if query.RecentMessages != 1 || !strings.Contains(query.Query, "earlier request") {
		t.Fatalf("memory query = %+v, want the earlier request as its only recent message", query)
	}
	if strings.Contains(query.Query, "[header]") {
		t.Fatalf("memory query repeated the persisted current message: %q", query.Query)
	}
}
