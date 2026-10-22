package agent

import (
	"context"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/agentapi"
	"github.com/felinics/memoh/internal/agent/internal/persist"
	"github.com/felinics/memoh/internal/agent/internal/web"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// These tests run a Web retry and a Web edit the way the WebSocket does,
// against the PostgreSQL ledger, and read back the turn that replaced the
// latest one. The run is fenced, so it commits its steps, and its first step
// replaces the old turn in the same transaction that writes the new one. That
// is the only rewrite of the old turn: a second one finds no visible turn left
// to replace and fails the run.

// countingReplacements counts the ways a turn is rewritten: a standalone
// ReplaceTurn, and a step that starts a replacement.
type countingReplacements struct {
	*messagepkg.DBService
	replaced atomic.Int32
	started  atomic.Int32
}

func (c *countingReplacements) ReplaceTurn(ctx context.Context, sessionID, oldTurnID, replacementTurnID string, replacementTurnPosition *int64, requestMessageID, assistantMessageID, reason string) (messagepkg.HistoryTurn, error) {
	c.replaced.Add(1)
	return c.DBService.ReplaceTurn(ctx, sessionID, oldTurnID, replacementTurnID, replacementTurnPosition, requestMessageID, assistantMessageID, reason)
}

func (c *countingReplacements) PersistAgentStep(ctx context.Context, step messagepkg.AgentStep) ([]messagepkg.Message, error) {
	if step.Replacement != nil {
		c.started.Add(1)
	}
	return c.DBService.PersistAgentStep(ctx, step)
}

func (c *countingReplacements) rewrites() string {
	return "replace=" + strconv.Itoa(int(c.replaced.Load())) + " started=" + strconv.Itoa(int(c.started.Load()))
}

type wsReplacementHarness struct {
	wsStepHistoryHarness
	counter *countingReplacements
}

// newWSReplacementHarness has the session's first turn answered with
// ANSWER-1, and counts the rewrites that follow it.
func newWSReplacementHarness(t *testing.T) wsReplacementHarness {
	t.Helper()
	counter := &countingReplacements{}
	h := newWSStepHistoryHarness(t, wsStepHistoryNumbered, wsStepHistoryMessages(func(store *messagepkg.DBService) messagepkg.Service {
		counter.DBService = store
		return counter
	}))
	first := h.run(t, nil)
	if first.sessionRun[0] != "completed" {
		t.Fatalf("first turn session_runs = %q, want completed", first.sessionRun)
	}
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
func (h wsReplacementHarness) replace(t *testing.T, start func(*web.Admission, agentapi.RunDelivery)) wsStepHistoryRun {
	t.Helper()
	admission, err := admitWebRun(context.Background(), h.service, agentapi.WebAdmitInput{
		BotID:        h.botID,
		SessionID:    h.sessionID,
		InvocationID: uuid.NewString(),
		Payload:      []byte(`{"kind":"replacement"}`),
	})
	if err != nil {
		t.Fatalf("AdmitWebRun() error = %v", err)
	}
	if admission.Duplicate() {
		t.Fatal("AdmitWebRun() did not start the run")
	}
	delivery := &wsStepHistoryDelivery{done: make(chan struct{})}
	start(admission, delivery)
	select {
	case <-delivery.done:
	case <-time.After(20 * time.Second):
		t.Fatal("replacement turn did not finish")
	}
	out := h.read(t, admission.RunID())
	out.failureFrames = delivery.failureFrames
	return out
}

func (h wsReplacementHarness) retry(t *testing.T) wsStepHistoryRun {
	t.Helper()
	target := h.latestTurnID(t)
	return h.replace(t, func(admission *web.Admission, delivery agentapi.RunDelivery) {
		h.service.RunWebRetry(admission, agentapi.WebRetryInput{
			BotID: h.botID, SessionID: h.sessionID, TargetTurnID: target,
		}, delivery)
	})
}

func (h wsReplacementHarness) edit(t *testing.T, text string) wsStepHistoryRun {
	t.Helper()
	target := h.latestTurnID(t)
	return h.replace(t, func(admission *web.Admission, delivery agentapi.RunDelivery) {
		h.service.RunWebEdit(admission, agentapi.WebEditInput{
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
	if rewrites := h.counter.rewrites(); rewrites != "replace=0 started=1" {
		t.Errorf("rewrites = %s, want the first step's replacement only", rewrites)
	}
	// The retry clones its request into the new turn; the old request and
	// answer stay in the table, hidden with the turn they belonged to.
	assertStrings(t, "history rows", h.rowVisibility(t), []string{
		`assistant "ANSWER-1" hidden`,
		`assistant "ANSWER-2"`,
		`user "` + directLifecyclePrompt + `"`,
		`user "` + directLifecyclePrompt + `" hidden`,
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
	if rewrites := h.counter.rewrites(); rewrites != "replace=0 started=1" {
		t.Errorf("rewrites = %s, want the first step's replacement only", rewrites)
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

// wsStepHistoryMessages wraps the message store of the service.
func wsStepHistoryMessages(wrap func(*messagepkg.DBService) messagepkg.Service) wsStepHistoryOption {
	return func(s *wsStepHistorySetup) { s.messages = wrap }
}
