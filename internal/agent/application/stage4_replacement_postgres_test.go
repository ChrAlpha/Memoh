package application

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	"github.com/felinics/memoh/internal/agent/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// promptRecorder keeps the body of every summarizer request, so a test can
// read what the summarizer was told.
type promptRecorder struct {
	inner  *stage4Summarizer
	bodies []string
}

func (p *promptRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		body, _ := io.ReadAll(req.Body)
		p.bodies = append(p.bodies, string(body))
		req.Body = io.NopCloser(strings.NewReader(string(body)))
	}
	return p.inner.RoundTrip(req)
}

// A replacement compacts only the raw history before the replaced turn, and
// the summaries it takes as prior are the ones that end before that turn.
// The late summary below covers rows after the cutoff, so it must not reach
// the prompt of a compaction bounded by the cutoff. The control run, with the
// same rows and no cutoff, shows the late summary does reach the prompt
// without the bound (O8, real PostgreSQL artifacts).
func TestStage4ReplacementCompactionIgnoresSummariesAfterTheCutoff(t *testing.T) {
	t.Run("cutoff bounds the prior and the rows", func(t *testing.T) {
		s, cutoffMs := newStage4LateSummaryScenario(t)
		cutoffCfg := s.triggerConfig(1)
		cutoffCfg.HistoryCutoff = &compaction.HistoryCutoff{BeforeMs: cutoffMs}
		recorder := &promptRecorder{inner: s.summarizer}
		cutoffCfg.HTTPClient = &http.Client{Transport: recorder}
		res, err := s.compaction.RunCompactionSync(s.ctx, cutoffCfg)
		if err != nil {
			t.Fatalf("replacement compaction: %v", err)
		}
		if res.Status != compaction.StatusOK {
			t.Fatalf("replacement compaction = %+v, want ok: the raw rows before the cutoff are uncovered", res)
		}
		if len(recorder.bodies) == 0 {
			t.Fatal("replacement compaction never reached the summarizer")
		}
		prompt := recorder.bodies[len(recorder.bodies)-1]
		if !strings.Contains(prompt, "a-00") {
			t.Fatal("replacement compaction did not summarize the raw rows before the cutoff")
		}
		if strings.Contains(prompt, "LATE-SUMMARY") {
			t.Fatal("replacement compaction carried a summary that ends after the cutoff as prior context")
		}
		if strings.Contains(prompt, "b-00") {
			t.Fatal("replacement compaction summarized rows after the cutoff")
		}
	})

	t.Run("control without the cutoff carries the late summary", func(t *testing.T) {
		s, _ := newStage4LateSummaryScenario(t)
		recorder := &promptRecorder{inner: s.summarizer}
		controlCfg := s.triggerConfig(1)
		controlCfg.HTTPClient = &http.Client{Transport: recorder}
		if _, err := s.compaction.RunCompactionSync(s.ctx, controlCfg); err != nil {
			t.Fatalf("control compaction: %v", err)
		}
		if len(recorder.bodies) == 0 {
			t.Fatal("control compaction never reached the summarizer; the test would pass without the cutoff being exercised")
		}
		if !strings.Contains(recorder.bodies[len(recorder.bodies)-1], "LATE-SUMMARY") {
			t.Fatal("control compaction did not carry the late summary as prior context; the stale-prior case is not reproduced")
		}
	})
}

// newStage4LateSummaryScenario leaves a-00 to a-05 raw before the cutoff row
// a-06, and covers b-00 and b-01 with a valid summary that ends after the
// cutoff. It returns the cutoff in milliseconds.
func newStage4LateSummaryScenario(t *testing.T) (*stage4Session, int64) {
	t.Helper()
	s := newStage4Session(t)
	s.persistConversation("a", 6)
	cutoffID := s.persist("user", "a-06")
	bIDs := s.persistConversation("b", 2)

	var startMs, endMs, cutoffMs int64
	if err := s.pool.QueryRow(s.ctx,
		`SELECT floor(extract(epoch from created_at) * 1000)::bigint FROM bot_history_messages WHERE id = $1`,
		bIDs[0]).Scan(&startMs); err != nil {
		t.Fatalf("read late start: %v", err)
	}
	if err := s.pool.QueryRow(s.ctx,
		`SELECT floor(extract(epoch from created_at) * 1000)::bigint FROM bot_history_messages WHERE id = $1`,
		bIDs[1]).Scan(&endMs); err != nil {
		t.Fatalf("read late end: %v", err)
	}
	if err := s.pool.QueryRow(s.ctx,
		`SELECT floor(extract(epoch from created_at) * 1000)::bigint FROM bot_history_messages WHERE id = $1`,
		cutoffID).Scan(&cutoffMs); err != nil {
		t.Fatalf("read cutoff: %v", err)
	}
	var lateID string
	if err := s.pool.QueryRow(s.ctx, `
		INSERT INTO bot_history_message_compacts (bot_id, session_id, status, summary, anchor_start_ms, anchor_end_ms)
		VALUES ($1, $2, 'ok', 'LATE-SUMMARY', $3, $4)
		RETURNING id::text
	`, s.botID, s.sessionID, startMs, endMs).Scan(&lateID); err != nil {
		t.Fatalf("insert late summary: %v", err)
	}
	if _, err := s.pool.Exec(s.ctx,
		`UPDATE bot_history_messages SET compact_id = $1 WHERE id = ANY($2::uuid[])`,
		lateID, bIDs); err != nil {
		t.Fatalf("link late rows: %v", err)
	}
	return s, cutoffMs
}

// A long session compacts its history, then a retry replaces the last turn.
// The summary of the history before that turn stays in the next round's
// context, the rows it covers are not sent again, and nothing of the replaced
// turn is summarized.
func TestStage4RetryKeepsSummariesBeforeTheReplacedTurn(t *testing.T) {
	s := newStage4Session(t)
	s.persistConversation("a", 8)
	if res := s.compact(1); res.Status != "ok" {
		t.Fatalf("first compaction status = %q, want ok", res.Status)
	}
	covered, _ := s.compactedLabels()
	if len(covered) == 0 {
		t.Fatal("first compaction covered no rows")
	}
	turn := s.persistConversation("b", 2)

	oldTurn, err := s.messages.GetVisibleTurnByMessage(s.ctx, s.sessionID, turn[1])
	if err != nil {
		t.Fatalf("get replaced turn: %v", err)
	}
	content, err := json.Marshal(runreq.SDKMessagesToModelMessages([]sdk.Message{sdk.AssistantMessage("c-00 " + strings.Repeat("padding ", s.padding))})[0])
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := s.messages.Persist(s.ctx, messagepkg.PersistInput{
		BotID: s.botID, SessionID: s.sessionID, Role: "assistant", Content: content,
		DisplayText: "c-00", SkipHistoryTurn: true,
	})
	if err != nil {
		t.Fatalf("persist replacement: %v", err)
	}
	position := oldTurn.Position + 1
	if _, err := s.messages.ReplaceTurn(s.ctx, s.sessionID, oldTurn.ID, uuid.NewString(), &position, turn[0], replacement.ID, "retry"); err != nil {
		t.Fatalf("replace turn: %v", err)
	}

	var epoch int64
	if err := s.pool.QueryRow(s.ctx, `SELECT compaction_epoch FROM bot_sessions WHERE id = $1`, s.sessionID).Scan(&epoch); err != nil {
		t.Fatalf("read session epoch: %v", err)
	}
	if epoch != 0 {
		t.Fatalf("session epoch = %d, want 0: a retry must not advance the epoch", epoch)
	}

	labels := stage4Labels(s.assemble(runreq.ChatRequest{}).Messages)
	if labels[0] != "SUMMARY-1" {
		t.Fatalf("next round context = %v, want the first summary leading", labels)
	}
	for _, label := range covered {
		if slices.Contains(labels, label) {
			t.Fatalf("next round context re-sends covered row %q: %v", label, labels)
		}
	}
	if slices.Contains(labels, "b-01") {
		t.Fatalf("next round context still holds the replaced assistant: %v", labels)
	}
	if !slices.Contains(labels, "c-00") {
		t.Fatalf("next round context lacks the replacement: %v", labels)
	}
}
