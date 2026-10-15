package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/turn"
)

func TestPostgresChatContextSwitchCancellationKeepsCommittedEffects(t *testing.T) {
	s := newContextSwitchScript(t, 24, 4200)
	f := s.f
	started := make(chan struct{})
	var once sync.Once
	s.summary = func(_ http.ResponseWriter, r *http.Request, _ []byte) {
		once.Do(func() { close(started) })
		<-r.Context().Done()
	}
	handle := startBoundaryChat(t, s)
	go func() {
		select {
		case <-started:
			handle.Cancel()
		case <-f.ctx.Done():
		}
	}()
	terminal, _ := collectBoundaryChat(t, handle)
	if terminal.State != "" && terminal.State != turn.RunStateAborted || f.summaries.Load() != 1 || f.modelCalls.Load() >= int32(s.steps) {
		t.Fatalf("cancellation did not stop the same run at its summary: terminal=%+v summaries=%d calls=%d", terminal, f.summaries.Load(), f.modelCalls.Load())
	}
	assertBoundaryFactsAndNextRound(t, s, handle.RunID(), "aborted")
}

func TestPostgresChatContextSwitchUnproductiveSummaryFailsBoundedly(t *testing.T) {
	for _, mode := range []string{"disabled", "empty"} {
		t.Run(mode, func(t *testing.T) {
			s := newContextSwitchScript(t, 24, 4200)
			f := s.f
			if mode == "disabled" {
				if _, err := f.pool.Exec(f.ctx, `UPDATE bots SET compaction_enabled=false WHERE id=$1`, f.botID); err != nil {
					t.Fatal(err)
				}
			} else {
				s.summary = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprint(w, `{"id":"empty-summary","object":"chat.completion","model":"compact-model","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`)
				}
			}
			handle := startBoundaryChat(t, s)
			terminal, codes := collectBoundaryChat(t, handle)
			if terminal.State != turn.RunStateFailed || len(codes) != 1 || !strings.HasPrefix(codes[0], "context.") {
				t.Fatalf("unproductive recovery did not fail explicitly: terminal=%+v error_codes=%v", terminal, codes)
			}
			if f.summaries.Load() > 3 || f.modelCalls.Load() >= int32(s.steps) {
				t.Fatalf("unproductive recovery exceeded its bound: summaries=%d calls=%d", f.summaries.Load(), f.modelCalls.Load())
			}
			if mode == "empty" && f.summaries.Load() == 0 || mode == "disabled" && f.summaries.Load() != 0 {
				t.Fatalf("summary scenario was not exercised: mode=%s summaries=%d", mode, f.summaries.Load())
			}
			assertBoundaryFactsAndNextRound(t, s, handle.RunID(), "failed")
		})
	}
}

func startBoundaryChat(t *testing.T, s *contextSwitchScript) turn.RunHandle {
	t.Helper()
	f := s.f
	if _, err := f.pool.Exec(f.ctx, `UPDATE bot_sessions SET type='chat' WHERE id=$1`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	handle, err := f.service.StartTurn(f.ctx, turn.StartTurnCommand{
		SchemaVersion: 1, TeamID: f.cfg.TeamID, Mode: turn.ModeChat,
		BotID: f.botID, ChatID: f.botID, ThreadID: f.sessionID,
		Query: switchOriginalInput, UserVisibleText: switchOriginalInput, CurrentChannel: "local",
		SkipMemoryExtraction: true, SkipTitleGeneration: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(handle.Cancel)
	return handle
}

func collectBoundaryChat(t *testing.T, handle turn.RunHandle) (turn.RunTerminal, []string) {
	t.Helper()
	var terminal turn.RunTerminal
	var codes []string
	for frame := range handle.Events() {
		if result, ok := turn.RunTerminalFrom(frame); ok {
			terminal = result
		}
		var data event.StreamEvent
		if err := json.Unmarshal(frame.Payload, &data); err != nil {
			t.Fatal(err)
		}
		if data.Type == event.Error {
			codes = append(codes, data.Code)
		}
	}
	for range handle.Errs() {
	}
	return terminal, codes
}

func assertBoundaryFactsAndNextRound(t *testing.T, s *contextSwitchScript, runID, state string) {
	t.Helper()
	f := s.f
	before, err := os.ReadFile(s.effects)
	if err != nil {
		t.Fatal(err)
	}
	effects := strings.Fields(string(before))
	for i, effect := range effects {
		if effect != fmt.Sprint(i+1) {
			t.Fatalf("effect was replayed or reordered: %q", before)
		}
	}
	var toolRows, pending int
	var storedState string
	if err := f.pool.QueryRow(f.ctx, `SELECT state FROM session_runs WHERE run_id=$1`, runID).Scan(&storedState); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM bot_history_messages WHERE session_id=$1 AND role='tool'`, f.sessionID).Scan(&toolRows); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM bot_history_message_compacts WHERE session_id=$1 AND status='pending'`, f.sessionID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if len(effects) == 0 || len(effects) != toolRows || storedState != state || pending != 0 {
		t.Fatalf("boundary lost committed facts or left work pending: effects=%d tools=%d state=%s pending=%d", len(effects), toolRows, storedState, pending)
	}
	s.steps = 0
	s.summary = nil
	if _, err := f.pool.Exec(f.ctx, `UPDATE bots SET compaction_enabled=true WHERE id=$1`, f.botID); err != nil {
		t.Fatal(err)
	}
	s.chat(t, "next-round after interrupted recovery")
	after, err := os.ReadFile(s.effects)
	if err != nil || string(after) != string(before) {
		t.Fatalf("next round replayed effects: before=%q after=%q err=%v", before, after, err)
	}
	t.Logf("state=%s committed_effects=%d pending=0 summary_calls=%d next_round=answered replayed_effects=0", state, len(effects), f.summaries.Load())
}
