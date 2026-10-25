package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/chat/timeline"
)

// A session whose bot recorded its history before keeping it under
// checkpoints answers its first request after the upgrade, through every
// entry that renders history, without summarizing what no request could
// render: the checkpoint steps over the oldest of that history, the request
// opens with a notice of it, and only what the entry's load window holds is
// folded. A model request loads within its model's window, a Discuss
// composition within the admission cap, and a compaction steps over no more
// than the load that follows it. A retry or an edit renders its history under
// the checkpoint its replacement keeps, so its compaction steps over. The
// same history written under checkpoints is folded whole.
func TestPostgresHistoryFromBeforeCheckpointsIsSteppedOver(t *testing.T) {
	type outcome struct {
		code      string
		summaries int32
		request   string
		omitted   int64
	}
	const current = "current message"
	run := func(t *testing.T, entry runEntry, legacy bool) outcome {
		s := newEffectLoop(t, entry)
		f := s.f
		s.recordLegacyHistory(entry, 15, 2800)
		if entry.name == "retry" || entry.name == "edit" {
			// The turn it replaces was answered before the upgrade, by a
			// model whose window held all of that history.
			first := current
			if entry.name == "edit" {
				first = "DRAFT_BEFORE_EDIT"
			}
			s.window(200000)
			f.service.SetContextAbsoluteMaxTokens(200000)
			if code := s.chat(first); code != "" {
				t.Fatalf("the turn to replace failed with %s", code)
			}
			f.service.SetContextAbsoluteMaxTokens(16000)
			s.window(8000)
		}
		if legacy {
			if _, err := f.pool.Exec(f.ctx, `UPDATE bots SET history_checkpoints_since = clock_timestamp() WHERE id=$1`, f.botID); err != nil {
				t.Fatal(err)
			}
		}
		summaries := f.summaries.Load()
		var code string
		switch entry.name {
		case "retry":
			code = s.replaceLatest(s.replacement(""))
		case "edit":
			code = s.replaceLatest(s.replacement(current))
		case "discuss":
			// A batch the protected input cannot fit beside makes the
			// trigger compact.
			for i := range 11 {
				f.appendMessageAt(uuid.NewString(), fmt.Sprintf("batch-%d ", i)+strings.Repeat("x", 2800), time.Now().Truncate(time.Millisecond))
			}
			code = entry.send(s, current)
		default:
			code = entry.send(s, current)
		}
		request, _ := f.lastRequest.Load().(string)
		var omitted int64
		if err := f.pool.QueryRow(f.ctx, `SELECT COALESCE(c.omitted_count, 0) FROM bot_sessions s
			LEFT JOIN bot_history_checkpoints c ON c.id = s.history_checkpoint_id WHERE s.id=$1`, f.sessionID).Scan(&omitted); err != nil {
			t.Fatal(err)
		}
		return outcome{code: code, summaries: f.summaries.Load() - summaries, request: request, omitted: omitted}
	}

	for _, tc := range []struct {
		entry string
		// omitted counts the rows past the entry's load window: 20 past the
		// 32000 bytes of the 8000-token model, 8 past the 64000 of the
		// 16000-token admission cap.
		omitted int64
	}{{"web", 20}, {"im", 20}, {"discuss", 8}, {"retry", 20}, {"edit", 20}} {
		t.Run(tc.entry, func(t *testing.T) {
			entry := runEntryNamed(tc.entry)
			legacy := run(t, entry, true)
			if legacy.code != "" || !strings.Contains(legacy.request, current) {
				t.Fatalf("legacy session: code=%q, want the request answered", legacy.code)
			}
			if legacy.omitted != tc.omitted || !strings.Contains(legacy.request, "[History notice]") {
				t.Fatalf("legacy session: omitted=%d notice=%v, want the %d rows past the load window stepped over and noticed", legacy.omitted, strings.Contains(legacy.request, "[History notice]"), tc.omitted)
			}
			if strings.Contains(legacy.request, "legacy-0 ") || strings.Contains(legacy.request, "legacy-1 ") {
				t.Fatal("legacy session: the oldest history reached the provider")
			}

			whole := run(t, entry, false)
			if whole.code != "" || whole.omitted != 0 || strings.Contains(whole.request, "[History notice]") {
				t.Fatalf("history under checkpoints: code=%q omitted=%d, want it folded whole without a notice", whole.code, whole.omitted)
			}
			if legacy.summaries >= whole.summaries {
				t.Fatalf("summarizer calls: legacy=%d whole=%d, want fewer than folding everything", legacy.summaries, whole.summaries)
			}
		})
	}
}

// recordLegacyHistory writes count exchanges of size bytes each way as the
// entry's channel recorded them: rows alone from the web; from an IM or
// Discuss session, user rows that record the inbound events the IM pipeline
// renders and the Discuss cursor has consumed.
func (s *effectLoop) recordLegacyHistory(entry runEntry, count, size int) {
	f := s.f
	f.t.Helper()
	if entry.name != "im" && entry.name != "discuss" {
		f.persistHistory("legacy-", 2*count, size, time.Now().Add(-30*24*time.Hour))
		return
	}
	mode := ""
	if entry.name == "discuss" {
		mode = "discuss"
	}
	for i := range count {
		text := fmt.Sprintf("legacy-%d ", 2*i) + strings.Repeat("x", size)
		if entry.name == "discuss" {
			f.appendMessageAt(uuid.NewString(), text, time.Now().Truncate(time.Millisecond))
		} else {
			id, eventID := s.push(text)
			content, _ := json.Marshal(text)
			if _, err := f.messages.Persist(f.ctx, messagepkg.PersistInput{BotID: f.botID, SessionID: f.sessionID, ExternalMessageID: id, EventID: eventID, Role: "user", Content: content}); err != nil {
				f.t.Fatal(err)
			}
		}
		answer, err := historyfrag.MarshalStoredModelMessage(turn.ModelMessage{Role: "assistant", Content: turn.NewTextContent(fmt.Sprintf("legacy-%d ", 2*i+1) + strings.Repeat("x", size))})
		if err != nil {
			f.t.Fatal(err)
		}
		if _, err := f.messages.Persist(f.ctx, messagepkg.PersistInput{BotID: f.botID, SessionID: f.sessionID, Role: "assistant", Content: answer, SessionMode: mode}); err != nil {
			f.t.Fatal(err)
		}
	}
	if entry.name == "discuss" {
		if err := f.cursor.EventStore.UpsertDiscussCursor(f.ctx, f.sessionID, "default", "", "", timeline.ConsumedDiscussCursor(f.rc)); err != nil {
			f.t.Fatal(err)
		}
	}
}
