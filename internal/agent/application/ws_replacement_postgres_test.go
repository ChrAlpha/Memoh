package application

import (
	"context"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/persist"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// These tests run a Web retry and a Web edit the way the WebSocket does,
// against the PostgreSQL ledger, and read back the turn that replaced the
// latest one. The run is fenced, so it commits its steps, and the queue
// coordinator finalizes the replacement in the final step's transaction. That
// is the only rewrite of the old turn: a second one finds no visible turn left
// to replace and fails the run.

// countingReplacements counts the two ways a turn is rewritten.
type countingReplacements struct {
	*messagepkg.DBService
	replaced  atomic.Int32
	finalized atomic.Int32
}

func (c *countingReplacements) ReplaceTurn(ctx context.Context, sessionID, oldTurnID, replacementTurnID string, replacementTurnPosition *int64, requestMessageID, assistantMessageID, reason string) (messagepkg.HistoryTurn, error) {
	c.replaced.Add(1)
	return c.DBService.ReplaceTurn(ctx, sessionID, oldTurnID, replacementTurnID, replacementTurnPosition, requestMessageID, assistantMessageID, reason)
}

func (c *countingReplacements) FinalizeAgentReplacement(ctx context.Context, sessionID string, replacement messagepkg.TurnReplacement, requestMessageID, assistantMessageID string) error {
	c.finalized.Add(1)
	return c.DBService.FinalizeAgentReplacement(ctx, sessionID, replacement, requestMessageID, assistantMessageID)
}

func (c *countingReplacements) rewrites() string {
	return "replace=" + strconv.Itoa(int(c.replaced.Load())) + " finalize=" + strconv.Itoa(int(c.finalized.Load()))
}

type wsReplacementHarness struct {
	wsStepHistoryHarness
	counter *countingReplacements
}

// newWSReplacementHarness has the session's first turn answered with
// ANSWER-1, and counts the rewrites that follow it.
func newWSReplacementHarness(t *testing.T) wsReplacementHarness {
	t.Helper()
	h := newWSStepHistoryHarness(t, wsStepHistoryNumbered)
	first := h.run(t, nil)
	if first.sessionRun[0] != "completed" {
		t.Fatalf("first turn session_runs = %q, want completed", first.sessionRun)
	}
	counter := &countingReplacements{DBService: h.messages}
	h.service.messageService = counter
	rewireComponents(h.service)
	return wsReplacementHarness{wsStepHistoryHarness: h, counter: counter}
}

func (h wsReplacementHarness) latestTurnID(t *testing.T) string {
	t.Helper()
	turn, err := h.messages.GetLatestVisibleTurnBySession(context.Background(), h.sessionID)
	if err != nil {
		t.Fatalf("GetLatestVisibleTurnBySession() error = %v", err)
	}
	return turn.ID
}

// replace admits one replacement run and runs it to its terminal write.
func (h wsReplacementHarness) replace(t *testing.T, start func(*WebAdmission, drive.RunDelivery)) wsStepHistoryRun {
	t.Helper()
	admission, err := h.service.AdmitWebRun(context.Background(), WebAdmitInput{
		BotID:        h.botID,
		SessionID:    h.sessionID,
		InvocationID: uuid.NewString(),
		Payload:      []byte(`{"kind":"replacement"}`),
	})
	if err != nil {
		t.Fatalf("AdmitWebRun() error = %v", err)
	}
	if admission.Duplicate {
		t.Fatal("AdmitWebRun() did not start the run")
	}
	delivery := &wsStepHistoryDelivery{done: make(chan struct{})}
	start(admission, delivery)
	select {
	case <-delivery.done:
	case <-time.After(20 * time.Second):
		t.Fatal("replacement turn did not finish")
	}
	out := h.read(t, admission.RunID)
	out.failureFrames = delivery.failureFrames
	return out
}

func (h wsReplacementHarness) retry(t *testing.T) wsStepHistoryRun {
	t.Helper()
	target := h.latestTurnID(t)
	return h.replace(t, func(admission *WebAdmission, delivery drive.RunDelivery) {
		h.service.RunWebRetry(admission, RetryLatestMessageInput{
			BotID: h.botID, SessionID: h.sessionID, TargetTurnID: target,
		}, delivery)
	})
}

func (h wsReplacementHarness) edit(t *testing.T, text string) wsStepHistoryRun {
	t.Helper()
	target := h.latestTurnID(t)
	return h.replace(t, func(admission *WebAdmission, delivery drive.RunDelivery) {
		h.service.RunWebEdit(admission, EditLatestMessageInput{
			BotID: h.botID, SessionID: h.sessionID, TargetTurnID: target, Text: text,
		}, delivery)
	})
}

// rowVisibility is one line per history row, sorted: role, the text the user
// sees, and "hidden" when the row's turn is not visible.
func (h wsReplacementHarness) rowVisibility(t *testing.T) []string {
	t.Helper()
	rows, err := h.pool.Query(context.Background(), `
		SELECT role, content, COALESCE(display_text, ''), turn_visible
		FROM bot_history_messages
		WHERE session_id = $1
	`, h.sessionID)
	if err != nil {
		t.Fatalf("read history rows: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var (
			row     messagepkg.Message
			visible bool
		)
		if err := rows.Scan(&row.Role, &row.Content, &row.DisplayContent, &visible); err != nil {
			t.Fatalf("scan history row: %v", err)
		}
		text := historyRowText(row)
		if row.Role == "user" {
			text = persist.VisibleMessageText(row)
		}
		line := row.Role + " " + strconv.Quote(text)
		if !visible {
			line += " hidden"
		}
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read history rows: %v", err)
	}
	slices.Sort(out)
	return out
}

func TestPostgresWSStepHistoryRetry(t *testing.T) {
	h := newWSReplacementHarness(t)
	got := h.retry(t)
	if got.sessionRun != [3]string{"completed", "", ""} {
		t.Fatalf("session_runs = %q, failure frames %q", got.sessionRun, got.failureFrames)
	}
	if rewrites := h.counter.rewrites(); rewrites != "replace=0 finalize=1" {
		t.Errorf("rewrites = %s, want the coordinator's finalize only", rewrites)
	}
	// The request row moves to the new turn; the old answer stays in the
	// table, hidden with the turn it belonged to.
	assertStrings(t, "history rows", h.rowVisibility(t), []string{
		`assistant "ANSWER-1" hidden`,
		`assistant "ANSWER-2"`,
		`user "` + directLifecyclePrompt + `"`,
	})
	assertStrings(t, "history turns", got.turns, []string{
		`user "` + directLifecyclePrompt + `"`,
		`assistant [text "ANSWER-2"]`,
	})
}

func TestPostgresWSStepHistoryEdit(t *testing.T) {
	const edited = "EDITED_PROMPT"
	h := newWSReplacementHarness(t)
	got := h.edit(t, edited)
	if got.sessionRun != [3]string{"completed", "", ""} {
		t.Fatalf("session_runs = %q, failure frames %q", got.sessionRun, got.failureFrames)
	}
	if rewrites := h.counter.rewrites(); rewrites != "replace=0 finalize=1" {
		t.Errorf("rewrites = %s, want the coordinator's finalize only", rewrites)
	}
	// The whole old turn is hidden; the edited request and its answer are
	// the visible turn.
	assertStrings(t, "history rows", h.rowVisibility(t), []string{
		`assistant "ANSWER-1" hidden`,
		`assistant "ANSWER-2"`,
		`user "` + edited + `"`,
		`user "` + directLifecyclePrompt + `" hidden`,
	})
	assertStrings(t, "history turns", got.turns, []string{
		`user "` + edited + `"`,
		`assistant [text "ANSWER-2"]`,
	})
}
