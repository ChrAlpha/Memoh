package agenttest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	"github.com/felinics/memoh/internal/agent/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db/dbtest"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// CompactionSummarizer stands in for the summarization model behind the real
// compaction service: every request is answered with a numbered summary, or
// refused when failWith is set. The summaries are long enough that two of
// them exceed the budget that triggers frontier fusion.
type CompactionSummarizer struct {
	mu       sync.Mutex
	calls    int
	failWith int
	// requests holds the body of every request received.
	requests []string
}

func (s *CompactionSummarizer) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	s.mu.Lock()
	s.calls++
	n, failWith := s.calls, s.failWith
	s.requests = append(s.requests, string(body))
	s.mu.Unlock()
	header := http.Header{"Content-Type": []string{"application/json"}}
	if failWith != 0 {
		return &http.Response{
			StatusCode: failWith, Header: header, Request: req,
			Body: io.NopCloser(strings.NewReader(`{"error":{"message":"summarizer refused"}}`)),
		}, nil
	}
	respBody, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-compaction", "object": "chat.completion", "model": "compaction-summarizer",
		"choices": []map[string]any{{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": fmt.Sprintf("SUMMARY-%d %s", n, strings.Repeat("recap ", 200))},
		}},
	})
	return &http.Response{StatusCode: http.StatusOK, Header: header, Request: req, Body: io.NopCloser(strings.NewReader(string(respBody)))}, nil
}

// CallCount is the number of summarization requests received.
func (s *CompactionSummarizer) CallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// RequestText returns everything the summarizer was sent.
func (s *CompactionSummarizer) RequestText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.requests, "\n")
}

// RefuseWith makes every later request fail with status; zero answers again.
func (s *CompactionSummarizer) RefuseWith(status int) {
	s.mu.Lock()
	s.failWith = status
	s.mu.Unlock()
}

// CompactionSession is a session in real PostgreSQL: history is written through
// the real message service and compacted by the real compaction service, with
// only the summarization model replaced.
type CompactionSession struct {
	T          *testing.T
	Ctx        context.Context
	Pool       *pgxpool.Pool
	BotID      string
	SessionID  string
	Messages   *messagepkg.DBService
	Compaction *compaction.Service
	Summarizer *CompactionSummarizer
	// Queries is the real store behind Messages and Compaction.
	Queries dbstore.Queries
	Clock   time.Time
	// padding is how many filler words each row carries.
	Padding int
}

// OpenCompactionSession creates the bot and session fixture in the test
// database and returns a session on it.
func OpenCompactionSession(t *testing.T) *CompactionSession {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.OpenPostgres(t, ctx)
	botID, sessionID := CreateSessionFixture(t, ctx, pool)
	return NewCompactionSessionFor(t, ctx, pool, botID, sessionID)
}

// NewCompactionSessionFor returns a session on an existing bot and session.
func NewCompactionSessionFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, botID, sessionID string) *CompactionSession {
	t.Helper()
	queries := postgresstore.NewQueriesWithPool(pool, dbsqlc.New(pool))
	logger := slog.New(slog.DiscardHandler)
	messages := messagepkg.NewService(nil, queries)
	summarizer := &CompactionSummarizer{}
	return &CompactionSession{
		T: t, Ctx: ctx, Pool: pool, BotID: botID, SessionID: sessionID,
		Messages: messages, Compaction: compaction.NewService(logger, queries), Summarizer: summarizer,
		Queries: queries,
		Clock:   time.Now().Add(-time.Hour),
		Padding: 130,
	}
}

// persist writes one row whose text starts with label and is long enough that
// a handful of rows reach a small context budget.
// Persist writes one row whose text starts with label and is long enough that
// a handful of rows reach a small context budget.
func (s *CompactionSession) Persist(role, label string) string {
	s.T.Helper()
	text := label + " " + strings.Repeat("padding ", s.Padding)
	message := sdk.UserMessage(text)
	if role == "assistant" {
		message = sdk.AssistantMessage(text)
	}
	content, err := json.Marshal(runreq.SDKMessagesToModelMessages([]sdk.Message{message})[0])
	if err != nil {
		s.T.Fatal(err)
	}
	row, err := s.Messages.Persist(s.Ctx, messagepkg.PersistInput{
		BotID: s.BotID, SessionID: s.SessionID, Role: role, Content: content, DisplayText: label,
	})
	if err != nil {
		s.T.Fatalf("persist %s: %v", label, err)
	}
	s.Clock = s.Clock.Add(time.Minute)
	if _, err := s.Pool.Exec(s.Ctx, `UPDATE bot_history_messages SET created_at = $2 WHERE id = $1`, row.ID, s.Clock); err != nil {
		s.T.Fatal(err)
	}
	return row.ID
}

// persistConversation writes n alternating user/assistant rows labelled
// prefix-00, prefix-01, ... and returns their ids.
func (s *CompactionSession) PersistConversation(prefix string, n int) []string {
	s.T.Helper()
	ids := make([]string, 0, n)
	for i := range n {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		ids = append(ids, s.Persist(role, fmt.Sprintf("%s-%02d", prefix, i)))
	}
	return ids
}

// compact runs one real compaction of the session. targetTokens decides how
// many of the newest rows stay raw.
// Compact runs one real compaction of the session.
func (s *CompactionSession) Compact(targetTokens int) compaction.Result {
	s.T.Helper()
	res, err := s.Compaction.RunCompactionSync(s.Ctx, s.TriggerConfig(targetTokens))
	if err != nil {
		s.T.Fatalf("compaction: %v", err)
	}
	if res.Status != compaction.StatusOK {
		s.T.Fatalf("compaction status = %q, want ok", res.Status)
	}
	return res
}

func (s *CompactionSession) TriggerConfig(targetTokens int) compaction.TriggerConfig {
	return compaction.TriggerConfig{
		BotID: s.BotID, SessionID: s.SessionID,
		ModelID: "compaction-summarizer", ClientType: "openai-completions",
		APIKey: "test", BaseURL: "http://summarizer.invalid",
		HTTPClient:   &http.Client{Transport: s.Summarizer},
		TargetTokens: targetTokens, AllowFrontierFusion: true, ContextWindowTokens: 1000,
	}
}

// compactedLabels returns the labels of the rows an ok summary stands for and
// of the rows still raw, each in chronological order. The label is the first
// word of the row text.
func (s *CompactionSession) CompactedLabels() (covered, raw []string) {
	s.T.Helper()
	rows, err := s.Pool.Query(s.Ctx, `
		SELECT m.display_text, c.id IS NOT NULL AND c.status = 'ok'
		FROM bot_history_messages m
		LEFT JOIN bot_history_message_compacts c ON c.id = m.compact_id
		WHERE m.session_id = $1 ORDER BY m.created_at, m.id`, s.SessionID)
	if err != nil {
		s.T.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var label string
		var isCovered bool
		if err := rows.Scan(&label, &isCovered); err != nil {
			s.T.Fatal(err)
		}
		if isCovered {
			covered = append(covered, label)
		} else {
			raw = append(raw, label)
		}
	}
	if err := rows.Err(); err != nil {
		s.T.Fatal(err)
	}
	return covered, raw
}

// CompactionLabels reduces model messages to their label (first word) so an
// ordering can be compared; a summary keeps its SUMMARY-n marker.
func CompactionLabels(messages []runreq.ModelMessage) []string {
	labels := make([]string, 0, len(messages))
	for _, message := range messages {
		text := strings.TrimSpace(message.TextContent())
		if i := strings.Index(text, "SUMMARY-"); i >= 0 {
			labels = append(labels, text[i:i+len("SUMMARY-")+1])
			continue
		}
		labels = append(labels, strings.Fields(text + " ")[0])
	}
	return labels
}

// RequireCompactionSplit returns the labels on both sides of the compaction
// boundary and fails the test when either side is empty.
func RequireCompactionSplit(t *testing.T, s *CompactionSession) (covered, raw []string) {
	t.Helper()
	covered, raw = s.CompactedLabels()
	if len(covered) == 0 || len(raw) == 0 {
		t.Fatalf("fixture needs rows on both sides of the boundary, covered=%v raw=%v", covered, raw)
	}
	return covered, raw
}
