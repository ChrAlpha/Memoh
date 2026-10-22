package agent

import (
	"context"
	"testing"

	"github.com/felinics/memoh/internal/agent/sessionmode"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/schedule"
)

func storedRunMode(t *testing.T, h wsStepHistoryHarness) string {
	t.Helper()
	var mode string
	if err := h.pool.QueryRow(context.Background(), `
		SELECT mode FROM session_runs WHERE session_id = $1 ORDER BY created_at DESC LIMIT 1
	`, h.sessionID).Scan(&mode); err != nil {
		t.Fatalf("read the run's mode: %v", err)
	}
	return mode
}

// Each entry that admits a run records the mode it admits in, over the real
// ledger and its CHECK. The discuss entry is covered by the discuss tests, and
// a resumed run takes its mode from the interrupted one.
func TestPostgresAdmissionRecordsTheEntrysRunMode(t *testing.T) {
	t.Run("web send", func(t *testing.T) {
		h := newWSStepHistoryHarness(t, wsStepHistorySuccess)
		h.run(t, nil)
		if got := storedRunMode(t, h); got != sessionmode.Chat {
			t.Fatalf("web run mode = %q, want %q", got, sessionmode.Chat)
		}
	})
	t.Run("schedule fire", func(t *testing.T) {
		h := newWSStepHistoryHarness(t, wsStepHistorySuccess)
		if _, err := h.service.TriggerSchedule(context.Background(), h.botID, schedule.TriggerPayload{
			FireID: "mode-fire", SessionID: h.sessionID, Command: directLifecyclePrompt, OwnerUserID: "user-1",
		}, ""); err != nil {
			t.Fatalf("TriggerSchedule() error = %v", err)
		}
		if got := storedRunMode(t, h); got != sessionmode.Schedule {
			t.Fatalf("schedule run mode = %q, want %q", got, sessionmode.Schedule)
		}
	})
	t.Run("subagent", func(t *testing.T) {
		h := newWSStepHistoryHarness(t, wsStepHistoryBlock)
		_, _, finish, err := h.service.AdmitSubagentRun(context.Background(), h.botID, h.sessionID, "subagent:mode", []byte(`{"task":"mode"}`))
		if err != nil {
			t.Fatalf("AdmitSubagentRun() error = %v", err)
		}
		defer finish(tools.SubagentTerminal{OutcomeResolved: true, Outcome: tools.SpawnAttemptAbort})
		if got := storedRunMode(t, h); got != sessionmode.Subagent {
			t.Fatalf("subagent run mode = %q, want %q", got, sessionmode.Subagent)
		}
	})
}
