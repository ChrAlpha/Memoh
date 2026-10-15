package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

func TestPostgresDiscussCommitsBeforeNextProviderCall(t *testing.T) {
	for _, mode := range []string{"complete", "provider-failure", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s := newContextSwitchScript(t, 2, 80)
			f := s.f
			f.broadcaster.deltas = make(chan string, 16)
			paused := make(chan struct{})
			f.provider = func(w http.ResponseWriter, r *http.Request, body []byte, stream bool) {
				if stream && f.modelCalls.Load() == 2 {
					var storedTools int
					if err := f.pool.QueryRow(r.Context(), `SELECT count(*) FROM bot_history_messages WHERE session_id=$1 AND role='tool'`, f.sessionID).Scan(&storedTools); err != nil {
						t.Error(err)
					}
					if storedTools != 1 {
						t.Errorf("next provider call preceded its durable tool result: tools=%d", storedTools)
					}
					content, _ := json.Marshal("later input outside this run")
					if _, err := f.messages.Persist(r.Context(), messagepkg.PersistInput{BotID: f.botID, SessionID: f.sessionID, ExternalMessageID: "later-input", Role: "user", Content: content, SessionMode: "discuss"}); err != nil {
						t.Error(err)
					}
					if mode == "provider-failure" {
						http.Error(w, `{"error":{"message":"scripted rejection","type":"authentication_error"}}`, http.StatusUnauthorized)
						return
					}
					if mode == "cancel" {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprint(w, "data: {\"id\":\"partial\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"PARTIAL_DISCUSS\"},\"finish_reason\":null}]}\n\n")
						w.(http.Flusher).Flush()
						close(paused)
						<-r.Context().Done()
						return
					}
				}
				s.serve(w, r, body, stream)
			}
			f.appendMessage("discuss-step-input", switchOriginalInput)
			if mode == "cancel" {
				f.driver.NotifyRC(f.ctx, f.sessionID, f.rc, f.cfg)
				select {
				case <-paused:
				case <-f.ctx.Done():
					t.Fatal("provider never reached second step")
				}
			partial:
				for {
					select {
					case text := <-f.broadcaster.deltas:
						if text == "PARTIAL_DISCUSS" {
							break partial
						}
					case <-f.ctx.Done():
						t.Fatal("partial output never reached the discuss consumer")
					}
				}
				f.driver.StopAll()
			} else {
				code := f.trigger()
				if mode == "complete" && code != "" || mode == "provider-failure" && code != "agent.provider_auth_failed" {
					t.Fatalf("unexpected discuss result: %q", code)
				}
			}
			wantState, wantTools := "completed", 2
			switch mode {
			case "provider-failure":
				wantState, wantTools = "failed", 1
			case "cancel":
				wantState, wantTools = "aborted", 1
			}
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				var state string
				if err := f.pool.QueryRow(f.ctx, `SELECT state FROM session_runs WHERE session_id=$1`, f.sessionID).Scan(&state); err != nil {
					t.Fatal(err)
				}
				if state == wantState {
					break
				}
				select {
				case <-deadline.C:
					t.Fatalf("run did not reach %s: %s", wantState, state)
				case <-ticker.C:
				}
			}
			var tools, original, replies int
			if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FILTER (WHERE role='tool'),count(*) FILTER (WHERE role='user'),count(*) FILTER (WHERE content::text LIKE '%SWITCH_LOOP_COMPLETE%') FROM bot_history_messages WHERE session_id=$1`, f.sessionID).Scan(&tools, &original, &replies); err != nil {
				t.Fatal(err)
			}
			if tools != wantTools || original != 2 || mode == "complete" && replies != 1 {
				t.Fatalf("terminal duplicated or lost history: tools=%d original=%d replies=%d", tools, original, replies)
			}
			var foreignRows int
			if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM bot_history_messages m JOIN session_runs r ON r.session_id=m.session_id JOIN bot_history_messages original ON original.session_id=m.session_id AND original.source_message_id='discuss-step-input' WHERE m.session_id=$1 AND m.role IN ('assistant','tool') AND (m.run_id IS DISTINCT FROM r.run_id OR m.turn_id IS DISTINCT FROM original.turn_id)`, f.sessionID).Scan(&foreignRows); err != nil {
				t.Fatal(err)
			}
			if foreignRows != 0 {
				t.Fatalf("step rows lost admitted run or original input turn identity: %d", foreignRows)
			}
			if mode == "cancel" {
				var partialRows int
				if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM bot_history_messages WHERE session_id=$1 AND role='assistant' AND content::text LIKE '%PARTIAL_DISCUSS%' AND metadata->>'agent_step_interrupted'='true'`, f.sessionID).Scan(&partialRows); err != nil {
					t.Fatal(err)
				}
				if partialRows != 1 {
					t.Fatalf("cancelled discuss partial was not checkpointed exactly once: %d", partialRows)
				}
			}
		})
	}
}
