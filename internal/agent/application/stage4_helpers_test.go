package application

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
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

// stage4Summarizer stands in for the summarization model behind the real
// compaction service: every request is answered with a numbered summary, or
// refused when failWith is set. The summaries are long enough that two of
// them exceed the budget that triggers frontier fusion.
type stage4Summarizer struct {
	mu       sync.Mutex
	calls    int
	failWith int
	// requests holds the body of every request received.
	requests []string
}

func (s *stage4Summarizer) RoundTrip(req *http.Request) (*http.Response, error) {
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
		"id": "chatcmpl-stage4", "object": "chat.completion", "model": "stage4-summarizer",
		"choices": []map[string]any{{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": fmt.Sprintf("SUMMARY-%d %s", n, strings.Repeat("recap ", 200))},
		}},
	})
	return &http.Response{StatusCode: http.StatusOK, Header: header, Request: req, Body: io.NopCloser(strings.NewReader(string(respBody)))}, nil
}

func (s *stage4Summarizer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// requestText returns everything the summarizer was sent.
func (s *stage4Summarizer) requestText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.requests, "\n")
}

func (s *stage4Summarizer) refuseWith(status int) {
	s.mu.Lock()
	s.failWith = status
	s.mu.Unlock()
}

// stage4Session is a session in real PostgreSQL: history is written through
// the real message service and compacted by the real compaction service, with
// only the summarization model replaced.
type stage4Session struct {
	t          *testing.T
	ctx        context.Context
	pool       *pgxpool.Pool
	botID      string
	sessionID  string
	messages   *messagepkg.DBService
	compaction *compaction.Service
	summarizer *stage4Summarizer
	service    *Service
	clock      time.Time
	// padding is how many filler words each row carries.
	padding int
}

func newStage4Session(t *testing.T) *stage4Session {
	t.Helper()
	ctx := context.Background()
	pool := openTurnAdmissionPostgres(t, ctx)
	botID, sessionID := createTurnAdmissionFixture(t, ctx, pool)
	return newStage4SessionFor(t, ctx, pool, botID, sessionID)
}

func newStage4SessionFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, botID, sessionID string) *stage4Session {
	t.Helper()
	queries := postgresstore.NewQueriesWithPool(pool, dbsqlc.New(pool))
	logger := slog.New(slog.DiscardHandler)
	messages := messagepkg.NewService(nil, queries)
	summarizer := &stage4Summarizer{}
	return &stage4Session{
		t: t, ctx: ctx, pool: pool, botID: botID, sessionID: sessionID,
		messages: messages, compaction: compaction.NewService(logger, queries), summarizer: summarizer,
		service: &Service{queries: queries, messageService: messages, logger: logger},
		clock:   time.Now().Add(-time.Hour),
		padding: 130,
	}
}

// persist writes one row whose text starts with label and is long enough that
// a handful of rows reach a small context budget.
func (s *stage4Session) persist(role, label string) string {
	s.t.Helper()
	text := label + " " + strings.Repeat("padding ", s.padding)
	message := sdk.UserMessage(text)
	if role == "assistant" {
		message = sdk.AssistantMessage(text)
	}
	content, err := json.Marshal(sdkMessagesToModelMessages([]sdk.Message{message})[0])
	if err != nil {
		s.t.Fatal(err)
	}
	row, err := s.messages.Persist(s.ctx, messagepkg.PersistInput{
		BotID: s.botID, SessionID: s.sessionID, Role: role, Content: content, DisplayText: label,
	})
	if err != nil {
		s.t.Fatalf("persist %s: %v", label, err)
	}
	s.clock = s.clock.Add(time.Minute)
	if _, err := s.pool.Exec(s.ctx, `UPDATE bot_history_messages SET created_at = $2 WHERE id = $1`, row.ID, s.clock); err != nil {
		s.t.Fatal(err)
	}
	return row.ID
}

// persistConversation writes n alternating user/assistant rows labelled
// prefix-00, prefix-01, ... and returns their ids.
func (s *stage4Session) persistConversation(prefix string, n int) []string {
	s.t.Helper()
	ids := make([]string, 0, n)
	for i := range n {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		ids = append(ids, s.persist(role, fmt.Sprintf("%s-%02d", prefix, i)))
	}
	return ids
}

// compact runs one real compaction of the session. targetTokens decides how
// many of the newest rows stay raw.
func (s *stage4Session) compact(targetTokens int) compaction.Result {
	s.t.Helper()
	res, err := s.compaction.RunCompactionSync(s.ctx, s.triggerConfig(targetTokens))
	if err != nil {
		s.t.Fatalf("compaction: %v", err)
	}
	if res.Status != compaction.StatusOK {
		s.t.Fatalf("compaction status = %q, want ok", res.Status)
	}
	return res
}

func (s *stage4Session) triggerConfig(targetTokens int) compaction.TriggerConfig {
	return compaction.TriggerConfig{
		BotID: s.botID, SessionID: s.sessionID,
		ModelID: "stage4-summarizer", ClientType: "openai-completions",
		APIKey: "test", BaseURL: "http://summarizer.invalid",
		HTTPClient:   &http.Client{Transport: s.summarizer},
		TargetTokens: targetTokens, AllowFrontierFusion: true, ContextWindowTokens: 1000,
	}
}

// compactedLabels returns the labels of the rows an ok summary stands for and
// of the rows still raw, each in chronological order. The label is the first
// word of the row text.
func (s *stage4Session) compactedLabels() (covered, raw []string) {
	s.t.Helper()
	rows, err := s.pool.Query(s.ctx, `
		SELECT m.display_text, c.id IS NOT NULL AND c.status = 'ok'
		FROM bot_history_messages m
		LEFT JOIN bot_history_message_compacts c ON c.id = m.compact_id
		WHERE m.session_id = $1 ORDER BY m.created_at, m.id`, s.sessionID)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var label string
		var isCovered bool
		if err := rows.Scan(&label, &isCovered); err != nil {
			s.t.Fatal(err)
		}
		if isCovered {
			covered = append(covered, label)
		} else {
			raw = append(raw, label)
		}
	}
	if err := rows.Err(); err != nil {
		s.t.Fatal(err)
	}
	return covered, raw
}

func (s *stage4Session) assemble(chat ChatRequest) assembledContext {
	s.t.Helper()
	chat.BotID, chat.ChatID, chat.ThreadID = s.botID, s.botID, s.sessionID
	assembled, err := s.service.assembleContext(s.ctx, chatContextRequest(contextPurposeRun, chat, 100000))
	if err != nil {
		s.t.Fatalf("assembleContext: %v", err)
	}
	return assembled
}

// stage4Labels reduces model messages to their label (first word) so an
// ordering can be compared; a summary keeps its SUMMARY-n marker.
func stage4Labels(messages []ModelMessage) []string {
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
