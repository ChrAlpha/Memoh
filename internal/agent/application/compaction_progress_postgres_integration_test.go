package application

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

type countingSummarizer struct {
	summary          string
	calls, maxTokens int
}

func (s *countingSummarizer) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls++
	var limits struct {
		MaxTokens           int `json:"max_tokens"`
		MaxCompletionTokens int `json:"max_completion_tokens"`
	}
	if err := json.NewDecoder(req.Body).Decode(&limits); err != nil {
		return nil, err
	}
	s.maxTokens = max(limits.MaxTokens, limits.MaxCompletionTokens)
	content, _ := json.Marshal(s.summary)
	body := `{"id":"stub","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":` + string(content) + `},"finish_reason":"stop"}]}`
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

type progressFixture struct {
	t                *testing.T
	ctx              context.Context
	pool             *pgxpool.Pool
	queries          *dbsqlc.Queries
	messages         *messagepkg.DBService
	botID, sessionID string
}

func newProgressFixture(t *testing.T) (progressFixture, *postgresstore.Queries) {
	t.Helper()
	pool := openTurnAdmissionPostgres(t, t.Context())
	botID, sessionID := createTurnAdmissionFixture(t, t.Context(), pool)
	queries := dbsqlc.New(pool)
	store := postgresstore.NewQueries(queries)
	return progressFixture{t: t, ctx: t.Context(), pool: pool, queries: queries, messages: messagepkg.NewService(nil, store), botID: botID, sessionID: sessionID}, store
}

func (f progressFixture) persist(role string, content any) messagepkg.Message {
	f.t.Helper()
	raw, err := json.Marshal(map[string]any{"role": role, "content": content})
	if err != nil {
		f.t.Fatal(err)
	}
	msg, err := f.messages.Persist(f.ctx, messagepkg.PersistInput{BotID: f.botID, SessionID: f.sessionID, Role: role, Content: raw})
	if err != nil {
		f.t.Fatal(err)
	}
	return msg
}

func (f progressFixture) text(role, text string) messagepkg.Message { return f.persist(role, text) }

func (f progressFixture) reasoning() messagepkg.Message {
	return f.persist("assistant", []map[string]any{{"type": "reasoning", "text": "reasoning barrier"}})
}

func (f progressFixture) exchange(tool, id, text string) []messagepkg.Message {
	return []messagepkg.Message{
		f.persist("assistant", []map[string]any{{"type": "tool-call", "toolCallId": id, "toolName": tool, "input": map[string]any{"text": text}}}),
		f.persist("tool", []map[string]any{{"type": "tool-result", "toolCallId": id, "toolName": tool, "output": map[string]any{"type": "text", "value": text}}}),
	}
}

func (f progressFixture) claim(id string) (string, string) {
	f.t.Helper()
	var claim, status string
	if err := f.pool.QueryRow(f.ctx, `SELECT COALESCE(m.compact_id::text, ''), COALESCE(c.status, '')
		FROM bot_visible_history_messages m LEFT JOIN bot_history_message_compacts c ON c.id = m.compact_id
		WHERE m.id = $1`, id).Scan(&claim, &status); err != nil {
		f.t.Fatal(err)
	}
	return claim, status
}

func (f progressFixture) config(model *countingSummarizer) compaction.TriggerConfig {
	return compaction.TriggerConfig{
		BotID: f.botID, SessionID: f.sessionID,
		ModelID: "stub-model", ClientType: "openai-completions", APIKey: "test", BaseURL: "http://stub.invalid",
		HTTPClient: &http.Client{Transport: model}, TargetTokens: 50,
	}
}

func (f progressFixture) replay(labels map[string]string) []string {
	f.t.Helper()
	svc := &Service{logger: slog.New(slog.DiscardHandler), queries: postgresstore.NewQueries(f.queries), messageService: f.messages}
	records, err := svc.loadHistoryRecords(f.ctx, historyfrag.ScopeFallback{}, f.sessionID, 60*24, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	records, err = svc.replaceCompactedMessages(f.ctx, f.sessionID, contextfrag.Scope{BotID: f.botID, SessionID: f.sessionID}, records, compactionArtifactBoundary{})
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, record := range records {
		if record.SourceKind == historyfrag.SourceCompactionLog {
			out = append(out, "summary")
		} else {
			out = append(out, labels[record.DBMessageID])
		}
	}
	return out
}

func TestPostgresCompactionAdvancesPastTinyProductionPrefix(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%t", restart), func(t *testing.T) {
			f, store := newProgressFixture(t)
			labels := map[string]string{}
			for n := 0; n < 14; n++ {
				f.exchange("ask_user", strconv.Itoa(n), "继续吗？")
				if n == 4 || n == 9 {
					f.reasoning()
				}
			}
			tiny := f.text("user", "请帮我继续处理剩下的这些任务")
			labels[tiny.ID] = "tiny"
			f.reasoning()
			var later []messagepkg.Message
			for n := 0; n < 3; n++ {
				later = append(later, f.text("user", "later request"))
				later = append(later, f.exchange("exec", strconv.Itoa(n), strings.Repeat("later output. ", 200))...)
				later = append(later, f.text("assistant", strings.Repeat("later answer. ", 60)))
			}
			current := f.text("user", "current question")
			labels[current.ID] = "current"
			model := &countingSummarizer{summary: strings.Repeat("s", 4*254-21)}
			svc := compaction.NewService(slog.New(slog.DiscardHandler), store)
			for pass := 0; pass < 5; pass++ {
				if restart && pass > 0 {
					svc = compaction.NewService(slog.New(slog.DiscardHandler), store)
				}
				_, _ = svc.RunCompactionSync(f.ctx, f.config(model))
				if _, status := f.claim(later[0].ID); status == "ok" {
					break
				}
			}
			first, status := f.claim(later[0].ID)
			if status != "ok" || model.calls > 2 {
				t.Fatalf("later history not committed within five passes: status=%q calls=%d", status, model.calls)
			}
			for _, row := range later {
				if claim, status := f.claim(row.ID); claim != first || status != "ok" {
					t.Fatalf("later row %s has claim=%s status=%s", row.ID, claim, status)
				}
			}
			for _, row := range []messagepkg.Message{tiny, current} {
				if _, status := f.claim(row.ID); status == "ok" {
					t.Fatalf("retained row %s was summarized", row.ID)
				}
			}
			if replay := strings.Join(f.replay(labels), ","); !strings.HasSuffix(replay, "tiny,,summary,current") {
				t.Fatalf("replay reordered retained history: %s", replay)
			}
		})
	}
}
