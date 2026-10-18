package memory

import (
	"context"
	"log/slog"
	"strings"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/transcript"
	"github.com/felinics/memoh/internal/prune"
	"github.com/felinics/memoh/internal/textutil"
)

const (
	defaultQueryMaxBytes     = 1600
	defaultQueryMaxLines     = 40
	defaultQueryMessageRunes = 280
)

type query struct {
	Query          string
	Source         string
	RecentMessages int
	Truncated      bool
}

type queryBuilder struct {
	MaxRecentMessages int
	MaxBytes          int
	MaxLines          int
	MaxMessageRunes   int
}

func defaultQueryBuilder() queryBuilder {
	return queryBuilder{
		MaxRecentMessages: transcript.MemoryQueryRecentMessages,
		MaxBytes:          defaultQueryMaxBytes,
		MaxLines:          defaultQueryMaxLines,
		MaxMessageRunes:   defaultQueryMessageRunes,
	}
}

func (k *Keeper) buildQuery(ctx context.Context, req runreq.ChatRequest) query {
	builder := defaultQueryBuilder()
	text := strings.TrimSpace(req.Query)
	if text == "" {
		return query{}
	}
	assembled, err := k.transcript.Assemble(ctx, transcript.ForChat(transcript.PurposeMemoryQuery, req, 0))
	if err != nil {
		k.logger.WarnContext(ctx, "memory query history load failed",
			slog.String("bot_id", req.BotID),
			slog.String("chat_id", req.ChatID),
			slog.String("session_id", req.ThreadID),
			slog.Any("error", err),
		)
		return builder.Build(req, nil)
	}
	loaded := assembled.Records
	return builder.Build(req, loaded)
}

func (b queryBuilder) Build(req runreq.ChatRequest, history []historyfrag.HistoryRecord) query {
	current := strings.TrimSpace(req.Query)
	if current == "" {
		return query{}
	}
	b = b.withDefaults()
	recent := b.recentUserMessages(current, history)
	if len(recent) == 0 {
		return query{Query: b.prune(current).text, Source: "current_query"}
	}

	var sb strings.Builder
	sb.WriteString("Current user request:\n")
	sb.WriteString(current)
	sb.WriteString("\n\nRecent user context:\n")
	for _, msg := range recent {
		sb.WriteString("- ")
		sb.WriteString(msg)
		sb.WriteString("\n")
	}
	pruned := b.prune(strings.TrimSpace(sb.String()))
	return query{
		Query:          pruned.text,
		Source:         "current_query_with_history",
		RecentMessages: len(recent),
		Truncated:      pruned.truncated,
	}
}

func (b queryBuilder) recentUserMessages(current string, history []historyfrag.HistoryRecord) []string {
	if len(history) == 0 || b.MaxRecentMessages <= 0 {
		return nil
	}
	currentKey := normalizeQueryText(current)
	seen := map[string]bool{currentKey: true}
	reversed := make([]string, 0, b.MaxRecentMessages)
	for i := len(history) - 1; i >= 0 && len(reversed) < b.MaxRecentMessages; i-- {
		msg := history[i].ModelMessage
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "user") {
			continue
		}
		text := strings.TrimSpace(msg.TextContent())
		if text == "" {
			continue
		}
		key := normalizeQueryText(text)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		reversed = append(reversed, textutil.TruncateRunesWithSuffix(text, b.MaxMessageRunes, "..."))
	}
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed
}

type prunedQuery struct {
	text      string
	truncated bool
}

func (b queryBuilder) prune(text string) prunedQuery {
	text = strings.TrimSpace(text)
	if text == "" {
		return prunedQuery{}
	}
	pruned := prune.PruneWithEdges(text, "memory query", prune.Config{
		MaxBytes:  b.MaxBytes,
		MaxLines:  b.MaxLines,
		HeadBytes: max(1, b.MaxBytes*2/3),
		TailBytes: max(1, b.MaxBytes/3),
		HeadLines: max(1, b.MaxLines*2/3),
		TailLines: max(1, b.MaxLines/3),
		Marker:    "[memory query pruned]",
	})
	return prunedQuery{text: pruned, truncated: pruned != text}
}

func (b queryBuilder) withDefaults() queryBuilder {
	if b.MaxRecentMessages <= 0 {
		b.MaxRecentMessages = transcript.MemoryQueryRecentMessages
	}
	if b.MaxBytes <= 0 {
		b.MaxBytes = defaultQueryMaxBytes
	}
	if b.MaxLines <= 0 {
		b.MaxLines = defaultQueryMaxLines
	}
	if b.MaxMessageRunes <= 0 {
		b.MaxMessageRunes = defaultQueryMessageRunes
	}
	return b
}

func normalizeQueryText(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(text)), " "))
}
