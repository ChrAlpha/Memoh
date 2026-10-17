package memory

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/agenttest"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/transcript"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

type queryMessageService struct {
	agenttest.RecordingMessages
	messages []messagepkg.Message
}

func (s *queryMessageService) ListActiveSinceBySession(context.Context, string, time.Time) ([]messagepkg.Message, error) {
	return append([]messagepkg.Message(nil), s.messages...), nil
}

func (s *queryMessageService) ListActiveAfterCompactionBySessionWithinBytes(context.Context, string, int64) (messagepkg.SessionHistoryAfterCompaction, error) {
	return messagepkg.SessionHistoryAfterCompaction{Messages: append([]messagepkg.Message(nil), s.messages...)}, nil
}

func (s *queryMessageService) ListRecentUserBySession(_ context.Context, _ string, limit int) ([]messagepkg.Message, error) {
	var users []messagepkg.Message
	for _, message := range s.messages {
		if message.Role == "user" {
			users = append(users, message)
		}
	}
	if limit < len(users) {
		users = users[len(users)-limit:]
	}
	return users, nil
}

func (s *queryMessageService) ListTurnResponseSourcesSinceBySessionWithinBytes(context.Context, string, time.Time, int64) ([]messagepkg.Message, error) {
	return append([]messagepkg.Message(nil), s.messages...), nil
}

func (*queryMessageService) MeasureActiveBySession(context.Context, string, time.Time) (messagepkg.ActiveMessagesMeasure, error) {
	return messagepkg.ActiveMessagesMeasure{}, nil
}

func TestMemoryQueryBuilderCombinesRecentUserMessages(t *testing.T) {
	t.Parallel()

	builder := queryBuilder{MaxRecentMessages: 2, MaxBytes: 2000, MaxLines: 20, MaxMessageRunes: 200}
	query := builder.Build(runreq.ChatRequest{Query: "What should I pack?"}, []historyfrag.HistoryRecord{
		{ModelMessage: runreq.ModelMessage{Role: "user", Content: runreq.NewTextContent("I am visiting Berlin next week")}},
		{ModelMessage: runreq.ModelMessage{Role: "assistant", Content: runreq.NewTextContent("Sounds fun.")}},
		{ModelMessage: runreq.ModelMessage{Role: "user", Content: runreq.NewTextContent("I prefer tea over coffee")}},
		{ModelMessage: runreq.ModelMessage{Role: "user", Content: runreq.NewTextContent("What should I pack?")}},
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

	builder := queryBuilder{MaxRecentMessages: 1, MaxBytes: 120, MaxLines: 6, MaxMessageRunes: 500}
	query := builder.Build(runreq.ChatRequest{Query: strings.Repeat("current ", 30)}, []historyfrag.HistoryRecord{
		{ModelMessage: runreq.ModelMessage{Role: "user", Content: runreq.NewTextContent(strings.Repeat("history ", 30))}},
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

	query := defaultQueryBuilder().Build(runreq.ChatRequest{Query: "   "}, []historyfrag.HistoryRecord{
		{ModelMessage: runreq.ModelMessage{Role: "user", Content: runreq.NewTextContent("history")}},
	})
	if query.Query != "" {
		t.Fatalf("expected empty query, got %+v", query)
	}
}

func TestBuildMemoryQueryRespectsRetryCutoffForRawAndCompactedHistory(t *testing.T) {
	t.Parallel()

	botID := "00000000-0000-0000-0000-00000000b715"
	sessionID := "00000000-0000-0000-0000-00000000e715"
	compactID := "00000000-0000-0000-0000-00000000c715"
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	prior := agenttest.PersistedHistoryMessage(t, "prior-user", "user", "prior visible context")
	prior.BotID = botID
	prior.SessionID = sessionID
	prior.CreatedAt = base.Add(10 * time.Minute)
	cutoff := agenttest.PersistedHistoryMessage(t, "cutoff-user", "user", "edited-away request")
	cutoff.BotID = botID
	cutoff.SessionID = sessionID
	cutoff.CreatedAt = base.Add(20 * time.Minute)

	messages := &queryMessageService{messages: []messagepkg.Message{prior, cutoff}}
	queries := &agenttest.RecordingCompactionLog{Logs: []sqlc.BotHistoryMessageCompact{{
		ID:            agenttest.UUID(compactID),
		BotID:         agenttest.UUID(botID),
		SessionID:     agenttest.UUID(sessionID),
		Status:        "ok",
		Summary:       "edited-away compacted context",
		AnchorStartMs: base.Add(15 * time.Minute).UnixMilli(),
		AnchorEndMs:   cutoff.CreatedAt.UnixMilli(),
	}}}
	logger := slog.New(slog.DiscardHandler)
	keeper := New(Deps{
		Logger:     logger,
		Messages:   messages,
		Transcript: transcript.New(transcript.Deps{Logger: logger, Messages: messages, Queries: queries}),
	})
	query := keeper.buildQuery(context.Background(), runreq.ChatRequest{
		BotID:                        botID,
		ThreadID:                     sessionID,
		Query:                        "replacement request",
		HistoryCutoffBeforeMessageID: cutoff.ID,
	})

	if !strings.Contains(query.Query, "prior visible context") {
		t.Fatalf("memory query lost pre-cutoff context: %q", query.Query)
	}
	for _, excluded := range []string{"edited-away request", "edited-away compacted context"} {
		if strings.Contains(query.Query, excluded) {
			t.Fatalf("memory query resurrected %q across retry cutoff: %q", excluded, query.Query)
		}
	}
}
