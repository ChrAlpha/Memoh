package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/application"
	"github.com/felinics/memoh/internal/agent/context/compaction"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

const switchOriginalInput = "ORIGINAL_INPUT_KEEP_EXACT: finish the numbered effects"
const switchSteerInput = "STEER_INPUT_KEEP_EXACT: preserve every completed effect"

type switchToolInput struct {
	Index int `json:"index"`
}

type contextSwitchScript struct {
	f        *backlogFixture
	steps    int
	size     int
	effects  string
	mu       sync.Mutex
	requests []string
	steer    bool
}

func (s *contextSwitchScript) Tools(context.Context, tools.SessionContext) ([]toolexec.Tool, error) {
	return []toolexec.Tool{toolexec.Define("numbered_effect", "Record a numbered effect and return its findings.", func(ctx *toolexec.ToolExecContext, input switchToolInput) (sdk.ToolOutput, error) {
		file, err := os.OpenFile(s.effects, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return sdk.ToolOutput{}, err
		}
		_, err = fmt.Fprintf(file, "%d\n", input.Index)
		closeErr := file.Close()
		if err != nil {
			return sdk.ToolOutput{}, err
		}
		if closeErr != nil {
			return sdk.ToolOutput{}, closeErr
		}
		if s.steer && input.Index == 2 {
			_, err = s.f.service.EnqueueSteer(ctx, application.QueueInput{
				TeamID: s.f.cfg.TeamID, BotID: s.f.botID, SessionID: s.f.sessionID,
				InvocationID: "switch-steer", Text: switchSteerInput,
			})
			if err != nil {
				return sdk.ToolOutput{}, err
			}
		}
		return toolexec.OutputFromValue(map[string]any{"index": input.Index, "findings": s.output(input.Index)}), nil
	})}, nil
}

func (s *contextSwitchScript) output(index int) string {
	return fmt.Sprintf("TOOL_RESULT_%03d ", index) + strings.Repeat(fmt.Sprintf("finding %03d. ", index), s.size/13)
}

func (s *contextSwitchScript) serve(w http.ResponseWriter, _ *http.Request, body []byte, stream bool) {
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"summary","object":"chat.completion","model":"compact-model","choices":[{"index":0,"message":{"role":"assistant","content":"SWITCH_SUMMARY_%03d: earlier numbered effects completed."},"finish_reason":"stop"}]}`, s.f.summaries.Load())
		return
	}
	s.mu.Lock()
	s.requests = append(s.requests, string(body))
	call := len(s.requests)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	delta := map[string]any{"role": "assistant", "content": "SWITCH_LOOP_COMPLETE"}
	finish := "stop"
	if call <= s.steps {
		delta = map[string]any{
			"role": "assistant", "reasoning_content": fmt.Sprintf("REASONING_%03d inspect the next effect", call),
			"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("effect_%03d", call), "type": "function", "function": map[string]any{"name": "numbered_effect", "arguments": fmt.Sprintf(`{"index":%d}`, call)}}},
		}
		finish = "tool_calls"
	}
	for _, choice := range []map[string]any{{"index": 0, "delta": delta, "finish_reason": nil}, {"index": 0, "delta": map[string]any{}, "finish_reason": finish}} {
		chunk, _ := json.Marshal(map[string]any{"id": "switch", "object": "chat.completion.chunk", "model": "chat-model", "choices": []any{choice}})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

func newContextSwitchScript(t *testing.T, steps, size int) *contextSwitchScript {
	t.Helper()
	f := newBacklogFixture(t)
	s := &contextSwitchScript{f: f, steps: steps, size: size, effects: filepath.Join(t.TempDir(), "effects")}
	f.provider = s.serve
	f.agent.SetToolProviders([]tools.ToolProvider{s})
	if _, err := f.pool.Exec(f.ctx, `UPDATE models SET config=config || '{"compatibilities":["tool-call"]}'::jsonb WHERE id=$1`, f.chatModel); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *contextSwitchScript) assertEffects(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(s.effects)
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for index := 1; index <= s.steps; index++ {
		_, _ = fmt.Fprintf(&want, "%d\n", index)
	}
	if string(data) != want.String() {
		t.Fatalf("effects were lost, repeated, or reordered: %q", data)
	}
}

func TestPostgresDiscussSameRunCompactionContinuesLongToolLoop(t *testing.T) {
	s := newContextSwitchScript(t, 24, 4200)
	f := s.f
	f.appendMessage("original-input", switchOriginalInput)
	if code := f.trigger(); code != "" {
		t.Fatalf("same-run loop stopped: code=%s model_calls=%d summaries=%d", code, f.modelCalls.Load(), f.summaries.Load())
	}
	s.assertEffects(t)
	s.mu.Lock()
	requests := append([]string(nil), s.requests...)
	s.mu.Unlock()
	if len(requests) != s.steps+1 {
		t.Fatalf("model calls=%d, want %d", len(requests), s.steps+1)
	}
	switches := 0
	for i, request := range requests {
		if strings.Count(request, switchOriginalInput) != 1 {
			t.Fatalf("request %d lost or duplicated the original input", i+1)
		}
		if i > 0 && (!strings.Contains(request, fmt.Sprintf("effect_%03d", i)) || !strings.Contains(request, s.output(i))) {
			t.Fatalf("request %d lost or trimmed its latest complete tool exchange", i+1)
		}
		if !strings.Contains(request, "SWITCH_SUMMARY_") {
			for previous := 1; previous <= i; previous++ {
				if !strings.Contains(request, fmt.Sprintf("TOOL_RESULT_%03d ", previous)) {
					t.Fatalf("request %d dropped effect %d without a summary; model_calls=%d summaries=%d", i+1, previous, len(requests), f.summaries.Load())
				}
			}
		}
		if i > 0 && strings.Contains(request, "SWITCH_SUMMARY_") && len(request) < len(requests[i-1]) {
			switches++
		}
	}
	if got := f.summaries.Load(); got < 2 || got > int32(s.steps) {
		t.Fatalf("loop must switch context at least twice within the step bound: summaries=%d", got)
	}
	if switches < 2 {
		t.Fatalf("summaries did not produce at least two smaller provider contexts: switches=%d", switches)
	}
	var runs, completed, toolRows int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*),count(*) FILTER (WHERE state='completed') FROM session_runs WHERE session_id=$1`, f.sessionID).Scan(&runs, &completed); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM bot_history_messages WHERE session_id=$1 AND role='tool'`, f.sessionID).Scan(&toolRows); err != nil {
		t.Fatal(err)
	}
	if runs != 1 || completed != 1 || toolRows != s.steps {
		t.Fatalf("durability: runs=%d completed=%d tool_rows=%d", runs, completed, toolRows)
	}
	queries := postgresstore.NewQueriesWithPool(f.pool, sqlc.New(f.pool))
	frontier, err := compaction.NewArtifactProjection(queries).LoadActiveSession(f.ctx, compaction.ArtifactOwner{BotID: f.botID, SessionID: f.sessionID, SessionIDKnown: true})
	if err != nil || len(frontier.Issues) > 0 {
		t.Fatalf("invalid durable summary frontier: %v %+v", err, frontier.Issues)
	}
	f.appendMessage("next-round", "next-round after the long tool loop")
	f.answer("next round", "next-round after the long tool loop")
	s.assertEffects(t)
	t.Logf("steps=%d switches=%d summary_calls=%d provider_calls=%d single_run=1 effects_exact=1 next_round=answered", s.steps, switches, f.summaries.Load(), f.modelCalls.Load())
}
