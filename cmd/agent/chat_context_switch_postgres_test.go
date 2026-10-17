package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"

	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/chat/timeline"
)

const (
	loopInput   = "ORIGINAL_INPUT_KEEP_EXACT: finish the numbered effects"
	loopSummary = "Earlier backlog summarized."
)

type effectInput struct {
	Index int `json:"index"`
}

// effectLoop scripts a chat run that calls one numbered effect per step for
// its first steps, each returning a large result, and then answers.
type effectLoop struct {
	f        *backlogFixture
	steps    int
	offset   int
	pipeline *timeline.Pipeline
	mu       sync.Mutex
	effects  []int
	requests []string
}

func newEffectLoop(t *testing.T, im bool) *effectLoop {
	t.Helper()
	f := newBacklogFixture(t)
	s := &effectLoop{f: f}
	f.stream = s.serve
	f.agent.SetToolProviders([]tools.ToolProvider{s})
	if _, err := f.pool.Exec(f.ctx, `UPDATE models SET config=config || '{"compatibilities":["tool-call"]}'::jsonb WHERE id=$1`, f.chatModel); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE bot_sessions SET type='chat', channel_type='local' WHERE id=$1`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	if im {
		s.pipeline = timeline.NewPipeline(timeline.RenderParams{})
		f.service.SetPipeline(s.pipeline)
	}
	return s
}

func effectResult(index int) string {
	return fmt.Sprintf("TOOL_RESULT_%03d ", index) + strings.Repeat("x", 4200)
}

func (s *effectLoop) Tools(context.Context, tools.SessionContext) ([]toolexec.Tool, error) {
	return []toolexec.Tool{toolexec.Define("numbered_effect", "Record a numbered effect and return its findings.", func(_ *toolexec.ToolExecContext, input effectInput) (sdk.ToolOutput, error) {
		s.mu.Lock()
		s.effects = append(s.effects, input.Index)
		s.mu.Unlock()
		return toolexec.OutputFromValue(effectResult(input.Index)), nil
	})}, nil
}

func (s *effectLoop) serve(w http.ResponseWriter, body []byte) {
	s.mu.Lock()
	s.requests = append(s.requests, string(body))
	reply := fmt.Sprintf("REPLY_%03d", len(s.requests))
	call := len(s.requests) - s.offset
	effect := call <= s.steps
	s.mu.Unlock()
	delta, finish := map[string]any{"role": "assistant", "content": reply}, "stop"
	if effect {
		delta = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
			"index": 0, "id": fmt.Sprintf("effect_%03d", call), "type": "function",
			"function": map[string]any{"name": "numbered_effect", "arguments": fmt.Sprintf(`{"index":%d}`, call)},
		}}}
		finish = "tool_calls"
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, choice := range []map[string]any{{"index": 0, "delta": delta, "finish_reason": nil}, {"index": 0, "delta": map[string]any{}, "finish_reason": finish}} {
		chunk, _ := json.Marshal(map[string]any{"id": "loop", "object": "chat.completion.chunk", "choices": []any{choice}})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

// chat runs one turn from the web UI, or from an IM channel whose message
// reaches the run through the timeline pipeline, and reports its error code.
func (s *effectLoop) chat(text string) string {
	f := s.f
	f.t.Helper()
	cmd := turn.StartTurnCommand{TeamID: f.cfg.TeamID, Mode: turn.ModeChat, BotID: f.botID, ChatID: f.botID, ThreadID: f.sessionID, Query: text, CurrentChannel: "local"}
	if s.pipeline != nil {
		cmd.ExternalMessageID = uuid.NewString()
		at := time.Now().UnixMilli()
		s.pipeline.PushEvent(f.sessionID, timeline.MessageEvent{
			SessionID: f.sessionID, MessageID: cmd.ExternalMessageID, ReceivedAtMs: at, TimestampSec: at / 1000,
			Content: []timeline.ContentNode{{Type: "text", Text: text}}, Conversation: timeline.ConversationMeta{Channel: "telegram", ConversationType: "private"},
		})
	}
	handle, err := f.service.StartTurn(f.ctx, cmd)
	if err != nil {
		f.t.Fatal(err)
	}
	for range handle.Events() {
	}
	if err := <-handle.Errs(); err != nil {
		return string(apperror.CodeOf(err))
	}
	return ""
}

// firstKept is the oldest effect result a request still carries; the ones
// before it are gone from the request.
func firstKept(request string, upTo int) int {
	first := upTo + 1
	for index := upTo; index >= 1 && strings.Contains(request, fmt.Sprintf("TOOL_RESULT_%03d ", index)); index-- {
		first = index
	}
	return first
}

// A chat tool loop that outgrows the model window keeps going on a summary of
// its earlier steps: every effect runs once, each request keeps the input and
// the latest exchange verbatim and leaves out only results a summary replaced,
// and the next turn reads the same summary back without compacting again or
// replaying an effect.
func TestPostgresChatInRunCompaction(t *testing.T) {
	for _, entry := range []string{"web", "im"} {
		t.Run(entry, func(t *testing.T) {
			const steps = 14
			s := newEffectLoop(t, entry == "im")
			f := s.f
			if code := s.chat("EARLIER_TURN_KEEP"); code != "" {
				t.Fatalf("earlier turn failed with %s", code)
			}
			s.offset, s.steps = len(s.requests), steps
			if code := s.chat(loopInput); code != "" {
				t.Fatalf("run failed with %s; provider_calls=%d summary_calls=%d", code, len(s.requests), f.summaries.Load())
			}
			want := make([]int, steps)
			for i := range want {
				want[i] = i + 1
			}
			if !slices.Equal(s.effects, want) || len(s.requests) != s.offset+steps+1 {
				t.Fatalf("effects=%v provider_calls=%d, want each effect once in order and one call per step", s.effects, len(s.requests))
			}
			switches, first := 0, 1
			for i, request := range s.requests[s.offset:] {
				kept := firstKept(request, i)
				if strings.Contains(request, fmt.Sprintf("TOOL_RESULT_%03d ", kept-1)) || (i > 0 && kept > i) {
					t.Fatalf("request %d dropped an effect result out of order or lost the latest one (first kept %d)", i+1, kept)
				}
				for index := 1; index < kept; index++ {
					if strings.Contains(request, fmt.Sprintf("TOOL_RESULT_%03d ", index)) {
						t.Fatalf("request %d still carries folded result %d", i+1, index)
					}
				}
				summaries := strings.Count(request, loopSummary)
				if strings.Count(request, loopInput) != 1 || (kept > 1) != (summaries == 1) || summaries > 1 {
					t.Fatalf("request %d: input=%d summaries=%d first kept=%d, want the input once and a summary exactly when results are gone", i+1, strings.Count(request, loopInput), summaries, kept)
				}
				if kept < first {
					t.Fatalf("request %d brought back folded results", i+1)
				}
				if kept > first {
					switches++
				}
				first = kept
			}
			if switches < 2 || int(f.summaries.Load()) < switches {
				t.Fatalf("switches=%d summary_calls=%d, want the loop to fold at least twice", switches, f.summaries.Load())
			}
			var runs, completed, toolRows, foldedRows, rawInput int
			if err := f.pool.QueryRow(f.ctx, `SELECT count(*), count(*) FILTER (WHERE state='completed') FROM session_runs WHERE session_id=$1`, f.sessionID).Scan(&runs, &completed); err != nil {
				t.Fatal(err)
			}
			if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FILTER (WHERE h.role='tool'), count(*) FILTER (WHERE h.role='tool' AND c.status='ok'), count(*) FILTER (WHERE h.role='user' AND h.compact_id IS NULL AND h.content::text LIKE $2) FROM bot_history_messages h LEFT JOIN bot_history_message_compacts c ON c.id=h.compact_id WHERE h.session_id=$1`, f.sessionID, "%"+loopInput+"%").Scan(&toolRows, &foldedRows, &rawInput); err != nil {
				t.Fatal(err)
			}
			if runs != 2 || completed != 2 || toolRows != steps || foldedRows != first-1 || rawInput != 1 || f.activeSummaries() != 1 {
				t.Fatalf("runs=%d completed=%d tool_rows=%d folded=%d raw_input=%d active_summaries=%d, want both runs completed, the %d folded steps under one summary and the input raw", runs, completed, toolRows, foldedRows, rawInput, f.activeSummaries(), first-1)
			}

			summaries, calls := f.summaries.Load(), len(s.requests)
			if code := s.chat("NEXT_ROUND_QUESTION"); code != "" || len(s.requests) != calls+1 || f.summaries.Load() != summaries {
				t.Fatalf("next turn: code=%q provider_calls=%d summary_calls=%d, want one answered call without compacting", code, len(s.requests)-calls, f.summaries.Load()-summaries)
			}
			next := s.requests[calls]
			markers := []string{"EARLIER_TURN_KEEP", "REPLY_001", loopInput, loopSummary, fmt.Sprintf("TOOL_RESULT_%03d ", first), "NEXT_ROUND_QUESTION"}
			at := make([]int, len(markers))
			for i, marker := range markers {
				if strings.Count(next, marker) != 1 {
					t.Fatalf("next turn carries %q %d times, want once", marker, strings.Count(next, marker))
				}
				at[i] = strings.Index(next, marker)
			}
			if !slices.IsSorted(at) || firstKept(next, steps) != first || len(s.effects) != steps {
				t.Fatalf("next turn: marker offsets=%v first kept=%d effects=%d, want the turn before, the run as it ended, then the question", at, firstKept(next, steps), len(s.effects))
			}
		})
	}
}
