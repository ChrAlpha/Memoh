package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/application"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/chat/timeline"
	"github.com/felinics/memoh/internal/runtimefence"
	"github.com/felinics/memoh/internal/schedule"
)

const (
	loopInput   = "ORIGINAL_INPUT_KEEP_EXACT: finish the numbered effects"
	loopSummary = "Earlier backlog summarized."
	busyInput   = "BUSY_TURN_KEEP"
)

type effectInput struct {
	Index int `json:"index"`
}

// runEntry is one way a native run starts. The entries that fold are the
// in-run compaction coverage set; the others are declared outside it and must
// still end in a bounded, visible way.
type runEntry struct {
	name  string
	folds bool
	send  func(s *effectLoop, text string) string
}

var runEntries = []runEntry{
	{name: "web", folds: true, send: (*effectLoop).chat},
	{name: "im", folds: true, send: (*effectLoop).chat},
	{name: "trigger", folds: true, send: (*effectLoop).trigger},
	{name: "follow-up", folds: true, send: (*effectLoop).followUp},
	{name: "retry", send: (*effectLoop).retry},
	{name: "discuss", send: (*effectLoop).discuss},
}

// effectLoop scripts the provider for one native run at a time: the run whose
// input carries marker calls one numbered effect per step for its first steps
// calls, each returning a large result, then answers. Any other request is
// answered at once.
type effectLoop struct {
	f        *backlogFixture
	pipeline *timeline.Pipeline
	mu       sync.Mutex
	marker   string
	steps    int
	calls    int
	start    int
	hold     chan struct{}
	effects  []int
	requests []string
}

func newEffectLoop(t *testing.T, entry runEntry) *effectLoop {
	t.Helper()
	f := newBacklogFixture(t)
	s := &effectLoop{f: f}
	f.stream = s.serve
	f.agent.SetToolProviders([]tools.ToolProvider{s})
	if _, err := f.pool.Exec(f.ctx, `UPDATE models SET config=config || '{"compatibilities":["tool-call"]}'::jsonb WHERE id=$1`, f.chatModel); err != nil {
		t.Fatal(err)
	}
	if entry.name != "discuss" {
		if _, err := f.pool.Exec(f.ctx, `UPDATE bot_sessions SET type='chat', channel_type='local' WHERE id=$1`, f.sessionID); err != nil {
			t.Fatal(err)
		}
	}
	if entry.name == "im" {
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
	request := string(body)
	s.mu.Lock()
	s.requests = append(s.requests, request)
	reply := fmt.Sprintf("REPLY_%03d", len(s.requests))
	effect := 0
	if s.marker != "" && strings.Contains(request, s.marker) {
		s.calls++
		if s.calls <= s.steps {
			effect = s.calls
		}
	}
	id := fmt.Sprintf("effect_%03d_%03d", len(s.requests), effect)
	hold := s.hold
	s.mu.Unlock()
	if hold != nil && strings.Contains(request, busyInput) {
		<-hold
	}
	delta, finish := map[string]any{"role": "assistant", "content": reply}, "stop"
	if effect > 0 {
		delta = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
			"index": 0, "id": id, "type": "function",
			"function": map[string]any{"name": "numbered_effect", "arguments": fmt.Sprintf(`{"index":%d}`, effect)},
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

// run sends text through the entry as a run of steps effects and reports its
// error code with the provider requests the run made.
func (s *effectLoop) run(entry runEntry, text string, steps int) (string, []string) {
	s.mu.Lock()
	s.marker, s.steps, s.calls, s.effects, s.start = text, steps, 0, nil, len(s.requests)
	s.mu.Unlock()
	code := entry.send(s, text)
	s.mu.Lock()
	defer s.mu.Unlock()
	var requests []string
	for _, request := range s.requests[s.start:] {
		if strings.Contains(request, text) {
			requests = append(requests, request)
		}
	}
	return code, requests
}

func (s *effectLoop) command(text string) turn.StartTurnCommand {
	f := s.f
	return turn.StartTurnCommand{TeamID: f.cfg.TeamID, Mode: turn.ModeChat, BotID: f.botID, ChatID: f.botID, ThreadID: f.sessionID, Query: text, CurrentChannel: "local"}
}

// chat runs one turn from the web UI, or from an IM channel whose message
// reaches the run through the timeline pipeline, and reports its error code.
func (s *effectLoop) chat(text string) string {
	f := s.f
	f.t.Helper()
	cmd := s.command(text)
	if s.pipeline != nil {
		cmd.ExternalMessageID = uuid.NewString()
		at := time.Now().UnixMilli()
		s.pipeline.PushEvent(f.sessionID, timeline.MessageEvent{
			SessionID: f.sessionID, MessageID: cmd.ExternalMessageID, ReceivedAtMs: at, TimestampSec: at / 1000,
			Content: []timeline.ContentNode{{Type: "text", Text: text}}, Conversation: timeline.ConversationMeta{Channel: "telegram", ConversationType: "private"},
		})
	}
	return s.startTurn(cmd)
}

func (s *effectLoop) startTurn(cmd turn.StartTurnCommand) string {
	f := s.f
	f.t.Helper()
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

// trigger fires a schedule whose command is text.
func (s *effectLoop) trigger(text string) string {
	f := s.f
	f.t.Helper()
	_, err := f.service.TriggerSchedule(f.ctx, f.botID, schedule.TriggerPayload{FireID: uuid.NewString(), ID: uuid.NewString(), Name: "effects", Command: text, OwnerUserID: f.ownerUserID, SessionID: f.sessionID}, "")
	if err != nil {
		return string(apperror.CodeOf(err))
	}
	return ""
}

// followUp queues text behind a run in progress. The queued turn starts when
// that run ends and is driven by the server, so its code is read from its run
// record.
func (s *effectLoop) followUp(text string) string {
	f := s.f
	f.t.Helper()
	var runs int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM session_runs WHERE session_id=$1`, f.sessionID).Scan(&runs); err != nil {
		f.t.Fatal(err)
	}
	hold := make(chan struct{})
	s.mu.Lock()
	s.hold = hold
	s.mu.Unlock()
	busy, err := f.service.StartTurn(f.ctx, s.command(busyInput))
	if err != nil {
		f.t.Fatal(err)
	}
	cmd := s.command(text)
	if _, err := f.service.StartTurn(f.ctx, cmd); !errors.Is(err, turn.ErrSessionBusy) {
		f.t.Fatalf("a turn was admitted while another runs: %v", err)
	}
	if err := f.service.EnqueueDeferredTurn(f.ctx, cmd); err != nil {
		f.t.Fatal(err)
	}
	close(hold)
	for range busy.Events() {
	}
	if err := <-busy.Errs(); err != nil {
		f.t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var state, code string
		var count int
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM session_runs WHERE session_id=$1`, f.sessionID).Scan(&count); err != nil {
			f.t.Fatal(err)
		}
		if count >= runs+2 {
			if err := f.pool.QueryRow(f.ctx, `SELECT state, coalesce(error_code, '') FROM session_runs WHERE session_id=$1 ORDER BY turn_position DESC LIMIT 1`, f.sessionID).Scan(&state, &code); err != nil {
				f.t.Fatal(err)
			}
			switch ledger.State(state) {
			case ledger.StateCompleted, ledger.StateFailed, ledger.StateAborted, ledger.StateLost:
				return code
			}
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("the queued turn did not finish: runs=%d state=%q", count, state)
		}
	}
}

// retry answers text from the web UI, then replaces that turn the way the
// Web client does: the new run is admitted, streamed over the WS surface and
// finished in the session runtime by the caller.
func (s *effectLoop) retry(text string) string {
	f := s.f
	f.t.Helper()
	s.mu.Lock()
	steps := s.steps
	s.steps = 0
	s.mu.Unlock()
	if code := s.chat(text); code != "" {
		f.t.Fatalf("the turn to retry failed with %s", code)
	}
	s.mu.Lock()
	s.steps, s.calls, s.start = steps, 0, len(s.requests)
	s.mu.Unlock()
	var targetTurnID string
	if err := f.pool.QueryRow(f.ctx, `SELECT turn_id FROM bot_history_messages WHERE session_id=$1 AND turn_id IS NOT NULL ORDER BY created_at DESC, id DESC LIMIT 1`, f.sessionID).Scan(&targetTurnID); err != nil {
		f.t.Fatal(err)
	}
	ctx, cancelCause := context.WithCancelCause(f.ctx)
	defer cancelCause(context.Canceled)
	abortCh := make(chan struct{}, 1)
	admission, err := f.manager.Admit(ctx, sessionruntime.AdmitInput{
		BotID: f.botID, SessionID: f.sessionID, InvocationID: uuid.NewString(), Payload: []byte(`{"kind":"retry_message"}`),
		Execution: sessionruntime.Execution{
			Admission: func(context.Context, sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error) {
				return sessionruntime.RunAdmissionView{}, nil
			},
			AbortCh: abortCh, Cancel: func() { cancelCause(context.Canceled) }, OwnershipCancel: cancelCause,
		},
	})
	if err != nil || !admission.Started {
		f.t.Fatalf("admit retry: started=%v err=%v", admission.Started, err)
	}
	handle := admission.Handle
	ctx = runtimefence.WithContext(ctx, runtimefence.Fence{BotID: handle.BotID, SessionID: handle.SessionID, Token: handle.FencingToken})
	position := admission.TurnPosition
	eventCh := make(chan application.WSStreamEvent, 256)
	forwarded := make(chan struct{})
	go func() {
		defer close(forwarded)
		for payload := range eventCh {
			var event native.StreamEvent
			if json.Unmarshal(payload, &event) == nil {
				_, _ = f.manager.HandleAgentEvent(f.ctx, handle, event)
			}
		}
	}()
	outcome, runErr := f.service.RetryLatestMessageWS(ctx, application.RetryLatestMessageInput{
		BotID: f.botID, SessionID: f.sessionID, RunID: admission.RunID, TurnID: admission.TurnID, TurnPosition: &position,
		TargetTurnID: targetTurnID, ActorChannelIdentityID: f.ownerUserID, ActorUserID: f.ownerUserID, RunHandle: handle,
	}, eventCh, abortCh)
	close(eventCh)
	<-forwarded
	switch {
	case runErr == nil:
	case errors.Is(runErr, context.Canceled):
		outcome = application.RunOutcome{}
	case outcome.Status == sessionruntime.RunStatusErrored && outcome.ErrorCode() != "":
	default:
		outcome = application.RunOutcome{Status: sessionruntime.RunStatusErrored, Cause: runErr}
	}
	if _, err := f.manager.FinishRunWithErrorCode(context.WithoutCancel(f.ctx), handle, outcome.Status, outcome.ErrorCode()); err != nil {
		f.t.Fatal(err)
	}
	return outcome.ErrorCode()
}

// discuss delivers text as an unanswered Discuss message and lets the driver
// answer it.
func (s *effectLoop) discuss(text string) string {
	s.f.appendMessage(uuid.NewString(), text)
	return s.f.trigger()
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

// A tool loop that outgrows the model window keeps going on a summary of its
// earlier steps in every entry of the coverage set: each effect runs once,
// each request keeps the input and the latest exchange verbatim and leaves
// out only results a summary replaced, and the next turn reads the same
// summary back without compacting again or replaying an effect. When the
// summarizer is down the loop ends visibly with budget_unsatisfied, keeps the
// steps it committed, and the next turn is answered once the summarizer is
// back. The entries outside the set never fold: they end bounded, without
// replaying an effect, and the next turn is answered.
func TestPostgresInRunCompactionByEntry(t *testing.T) {
	for _, entry := range runEntries {
		t.Run(entry.name, func(t *testing.T) {
			const steps = 14
			s := newEffectLoop(t, entry)
			f := s.f
			if code, _ := s.run(entry, "EARLIER_TURN_KEEP", 0); code != "" {
				t.Fatalf("earlier turn failed with %s", code)
			}
			earlierReply := fmt.Sprintf("REPLY_%03d", len(s.requests))
			summaries := f.summaries.Load()
			code, requests := s.run(entry, loopInput, steps)
			if !entry.folds {
				if f.summaries.Load() != summaries || (code != "" && code != "context.budget_unsatisfied") || !slices.IsSorted(s.effects) || len(slices.Compact(slices.Clone(s.effects))) != len(s.effects) || len(s.effects) > steps {
					t.Fatalf("outside the coverage set: code=%q summary_calls=%d effects=%v, want no fold, a bounded end and no replayed effect", code, f.summaries.Load()-summaries, s.effects)
				}
				t.Logf("code=%q provider_calls=%d effects=%d first_kept_in_last_request=%d", code, len(requests), len(s.effects), firstKept(requests[len(requests)-1], steps))
				if code, _ := s.run(entry, "NEXT_ROUND_QUESTION", 0); code != "" {
					t.Fatalf("next turn after the loop failed with %s", code)
				}
				return
			}
			if code != "" {
				t.Fatalf("run failed with %s; provider_calls=%d summary_calls=%d", code, len(requests), f.summaries.Load()-summaries)
			}
			want := make([]int, steps)
			for i := range want {
				want[i] = i + 1
			}
			if !slices.Equal(s.effects, want) || len(requests) != steps+1 {
				t.Fatalf("effects=%v provider_calls=%d, want each effect once in order and one call per step", s.effects, len(requests))
			}
			switches, first := 0, 1
			for i, request := range requests {
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
			if switches < 2 || int(f.summaries.Load()-summaries) < switches {
				t.Fatalf("switches=%d summary_calls=%d, want the loop to fold at least twice", switches, f.summaries.Load()-summaries)
			}
			var running, toolRows, foldedRows, rawInput int
			if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FILTER (WHERE state NOT IN ('completed')) FROM session_runs WHERE session_id=$1`, f.sessionID).Scan(&running); err != nil {
				t.Fatal(err)
			}
			if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FILTER (WHERE h.role='tool'), count(*) FILTER (WHERE h.role='tool' AND c.status='ok'), count(*) FILTER (WHERE h.role='user' AND h.compact_id IS NULL AND h.content::text LIKE $2) FROM bot_history_messages h LEFT JOIN bot_history_message_compacts c ON c.id=h.compact_id WHERE h.session_id=$1`, f.sessionID, "%"+loopInput+"%").Scan(&toolRows, &foldedRows, &rawInput); err != nil {
				t.Fatal(err)
			}
			if running != 0 || toolRows != steps || foldedRows != first-1 || rawInput != 1 || f.activeSummaries() != 1 {
				t.Fatalf("unfinished_runs=%d tool_rows=%d folded=%d raw_input=%d active_summaries=%d, want every run completed, the %d folded steps under one summary and the input raw", running, toolRows, foldedRows, rawInput, f.activeSummaries(), first-1)
			}

			summaries = f.summaries.Load()
			code, requests = s.run(entry, "NEXT_ROUND_QUESTION", 0)
			if code != "" || len(requests) != 1 || f.summaries.Load() != summaries {
				t.Fatalf("next turn: code=%q provider_calls=%d summary_calls=%d, want one answered call without compacting", code, len(requests), f.summaries.Load()-summaries)
			}
			next := requests[0]
			markers := []string{"EARLIER_TURN_KEEP", earlierReply, loopInput, loopSummary, fmt.Sprintf("TOOL_RESULT_%03d ", first), "NEXT_ROUND_QUESTION"}
			at := make([]int, len(markers))
			for i, marker := range markers {
				if strings.Count(next, marker) != 1 {
					t.Fatalf("next turn carries %q %d times, want once", marker, strings.Count(next, marker))
				}
				at[i] = strings.Index(next, marker)
			}
			if !slices.IsSorted(at) || firstKept(next, steps) != first || len(s.effects) != 0 {
				t.Fatalf("next turn: marker offsets=%v first kept=%d effects=%v, want the turn before, the run as it ended, then the question", at, firstKept(next, steps), s.effects)
			}

			f.failSummary.Store(true)
			summaries = f.summaries.Load()
			code, requests = s.run(entry, "SUMMARIZER_DOWN_INPUT", steps)
			if code != "context.budget_unsatisfied" || len(s.effects) == 0 || len(s.effects) >= steps || !slices.IsSorted(s.effects) || len(slices.Compact(slices.Clone(s.effects))) != len(s.effects) {
				t.Fatalf("summarizer down: code=%q summary_attempts=%d provider_calls=%d effects=%v, want budget_unsatisfied once a fold cannot help, with the effects so far run once", code, f.summaries.Load()-summaries, len(requests), s.effects)
			}
			// A fold the summarizer refuses and a fold with no room for a
			// summary both end here; which one it was is evidence, not
			// contract.
			t.Logf("summarizer down: provider_calls=%d summary_attempts=%d effects=%d", len(requests), f.summaries.Load()-summaries, len(s.effects))
			committed := len(s.effects)
			var failedRows int
			if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM bot_history_messages WHERE session_id=$1 AND role='tool' AND created_at >= (SELECT max(created_at) FROM bot_history_messages WHERE session_id=$1 AND role='user' AND content::text LIKE '%SUMMARIZER_DOWN_INPUT%')`, f.sessionID).Scan(&failedRows); err != nil {
				t.Fatal(err)
			}
			if failedRows != committed {
				t.Fatalf("summarizer down: tool rows after the input=%d, want the %d committed effects kept", failedRows, committed)
			}
			f.failSummary.Store(false)
			summaries = f.summaries.Load()
			if code, requests := s.run(entry, "AFTER_SUMMARIZER_BACK", 0); code != "" || len(requests) != 1 || f.summaries.Load() == summaries || len(s.effects) != 0 {
				t.Fatalf("summarizer back: code=%q provider_calls=%d summary_calls=%d effects=%v, want the turn answered on a fresh summary without replaying an effect", code, len(requests), f.summaries.Load()-summaries, s.effects)
			}
		})
	}
}
