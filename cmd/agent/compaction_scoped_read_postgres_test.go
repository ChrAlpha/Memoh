package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

func TestPostgresScopedCompactionPassesExcludedPrefixAndPreservesGaps(t *testing.T) {
	for _, gapKind := range []string{"unconsumed_input", "stored_summary"} {
		t.Run(gapKind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			pool := openDiscussRecoveryPostgres(t, ctx)
			botID, sessionID := createDiscussRecoveryFixture(t, ctx, pool)
			queries := postgresstore.NewQueriesWithPool(pool, sqlc.New(pool))
			logger := slog.New(slog.DiscardHandler)
			messages := messagepkg.NewService(logger, queries)
			persist := func(role, text string, metadata map[string]any) messagepkg.Message {
				content, _ := json.Marshal(text)
				written, err := messages.Persist(ctx, messagepkg.PersistInput{BotID: botID, SessionID: sessionID, Role: role, Content: content, Metadata: metadata, SessionMode: "discuss"})
				if err != nil {
					t.Fatal(err)
				}
				stored, err := messages.GetByIDBySession(ctx, sessionID, written.ID)
				if err != nil {
					t.Fatal(err)
				}
				return stored
			}
			head := persist("user", "old record", map[string]any{"diagnostic": strings.Repeat("d", 600<<10)})
			first := persist("assistant", strings.Repeat("completed first step ", 120), nil)
			gap := persist("user", strings.Repeat("retained instruction ", 100), nil)
			second := persist("assistant", strings.Repeat("completed second step ", 120), nil)
			ref := func(message messagepkg.Message) contextfrag.ContextRef {
				record, err := historyfrag.FromDBMessage(message, historyfrag.ScopeFallback{})
				if err != nil {
					t.Fatal(err)
				}
				return record.Ref
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"id":"summary","object":"chat.completion","model":"compact-model","choices":[{"index":0,"message":{"role":"assistant","content":"Completed work preserved."},"finish_reason":"stop"}]}`)
			}))
			defer server.Close()
			service := compaction.NewService(logger, queries)
			cfg := compaction.TriggerConfig{BotID: botID, SessionID: sessionID, ModelID: "compact-model", ClientType: "openai-completions", BaseURL: server.URL, TargetTokens: 200}
			if gapKind == "stored_summary" {
				cfg.EligibleSources = []contextfrag.ContextRef{ref(gap)}
				result, err := service.RunCompactionSync(ctx, cfg)
				if err != nil || result.Status != compaction.StatusOK {
					t.Fatalf("seed gap summary: %+v %v", result, err)
				}
			}
			cfg.EligibleSources = []contextfrag.ContextRef{ref(first), ref(second)}
			result, err := service.RunCompactionSync(ctx, cfg)
			if err != nil || result.Status != compaction.StatusOK || result.MessageCount != 1 {
				t.Fatalf("first scoped pass: result=%+v error=%v", result, err)
			}
			compactID := func(id string) string {
				var compact string
				if err := pool.QueryRow(ctx, `SELECT COALESCE(compact_id::text,'') FROM bot_history_messages WHERE id=$1 AND session_id=$2`, id, sessionID).Scan(&compact); err != nil {
					t.Fatal(err)
				}
				return compact
			}
			claimedFirst := compactID(first.ID)
			if claimedFirst == "" {
				t.Fatal("eligible first step not committed")
			}
			for _, original := range []messagepkg.Message{head, second} {
				if compactID(original.ID) != "" {
					t.Fatalf("first pass crossed gap or touched excluded head: id=%s", original.ID)
				}
			}
			cfg.EligibleSources = []contextfrag.ContextRef{ref(second)}
			result, err = service.RunCompactionSync(ctx, cfg)
			if err != nil || result.Status != compaction.StatusOK || result.MessageCount != 1 {
				t.Fatalf("next scoped pass failed to advance: %+v %v", result, err)
			}
			claimedSecond := compactID(second.ID)
			if claimedSecond == "" || claimedSecond == claimedFirst {
				t.Fatalf("two disjoint intervals shared one artifact: first=%s second=%s", claimedFirst, claimedSecond)
			}
			if gapKind == "unconsumed_input" && compactID(gap.ID) != "" {
				t.Fatal("unconsumed gap was compacted")
			}
			wantCalls := int32(2)
			if gapKind == "stored_summary" {
				wantCalls++
			}
			if calls.Load() != wantCalls {
				t.Fatalf("summary calls=%d want=%d", calls.Load(), wantCalls)
			}
		})
	}
}
