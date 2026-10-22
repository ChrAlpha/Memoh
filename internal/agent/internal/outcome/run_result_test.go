package outcome

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/apperror"
)

func TestRunResultRecordsHowTheRunEnded(t *testing.T) {
	t.Parallel()
	providerFailure := apperror.Wrap(apperror.CodeAgentProviderOverloaded, errors.New("upstream 503"), nil)
	tests := []struct {
		name      string
		terminal  sessionruntime.TerminalRun
		cause     error
		wantLevel slog.Level
		wantFault string
	}{
		{
			name:      "owner completed",
			terminal:  sessionruntime.TerminalRun{State: string(ledger.StateCompleted)},
			wantLevel: slog.LevelInfo,
		},
		{
			name: "owner failed with its cause",
			terminal: sessionruntime.TerminalRun{
				State: string(ledger.StateFailed), ErrorCode: string(apperror.CodeAgentProviderOverloaded),
			},
			cause:     providerFailure,
			wantLevel: slog.LevelError,
			wantFault: "dependency",
		},
		{
			name:      "stopped",
			terminal:  sessionruntime.TerminalRun{State: string(ledger.StateAborted)},
			cause:     context.Canceled,
			wantLevel: slog.LevelInfo,
		},
		{
			name: "lost and found by the reaper",
			terminal: sessionruntime.TerminalRun{
				State: string(ledger.StateLost), ErrorCode: string(apperror.CodeRuntimeOwnerLeaseExpired),
			},
			wantLevel: slog.LevelError,
			wantFault: "server",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, logs := agenttest.CaptureLogs()
			terminal := tt.terminal
			terminal.RunID, terminal.BotID, terminal.SessionID, terminal.Applied = "run-1", "bot-1", "session-1", true
			ctx := context.Background()
			if tt.cause != nil {
				ctx = WithRunOutcome(ctx, "run-1", RunOutcome{Cause: tt.cause})
			}

			LogRunResult(ctx, logger, terminal)

			records := logs.RunResults()
			if len(records) != 1 {
				t.Fatalf("result records = %d, want 1", len(records))
			}
			if records[0].Level != tt.wantLevel {
				t.Errorf("level = %v, want %v", records[0].Level, tt.wantLevel)
			}
			attrs := agenttest.RecordAttrs(records[0])
			want := map[string]string{
				"operation": "agent.run", "run_id": "run-1", "bot_id": "bot-1", "session_id": "session-1",
				"state": terminal.State, "error_code": terminal.ErrorCode,
			}
			for key, value := range want {
				if attrs[key] != value {
					t.Errorf("%s = %q, want %q", key, attrs[key], value)
				}
			}
			if attrs["fault"] != tt.wantFault {
				t.Errorf("fault = %q, want %q", attrs["fault"], tt.wantFault)
			}
			if tt.name == "owner failed with its cause" && attrs["error"] == "" {
				t.Error("a failed run's record does not carry its owner's cause")
			}
		})
	}
}

// A run nobody waits for (schedule, discuss, subagent) is asynchronous: a
// client fault in it is this process's to fix, so it is an ERROR. The same
// fault in a chat run is the user's, and the user sees it in the run's
// terminal frame.
func TestRunResultAttributesByTheAdmittedMode(t *testing.T) {
	t.Parallel()
	clientFailure := apperror.New(apperror.CodeACPModelUnavailable, nil)
	tests := []struct {
		mode      string
		wantLevel slog.Level
		wantFault string
	}{
		{mode: sessionmode.Chat, wantLevel: slog.LevelInfo, wantFault: "client"},
		{mode: "", wantLevel: slog.LevelInfo, wantFault: "client"},
		{mode: sessionmode.Schedule, wantLevel: slog.LevelError, wantFault: "server"},
		{mode: sessionmode.Discuss, wantLevel: slog.LevelError, wantFault: "server"},
		{mode: sessionmode.Subagent, wantLevel: slog.LevelError, wantFault: "server"},
	}
	for _, tt := range tests {
		t.Run("mode="+tt.mode, func(t *testing.T) {
			t.Parallel()
			logger, logs := agenttest.CaptureLogs()
			ctx := WithRunOutcomeInMode(context.Background(), "run-1", RunOutcome{Cause: clientFailure}, tt.mode)

			LogRunResult(ctx, logger, sessionruntime.TerminalRun{
				RunID: "run-1", State: string(ledger.StateFailed), ErrorCode: string(apperror.CodeACPModelUnavailable), Applied: true,
			})

			records := logs.RunResults()
			if len(records) != 1 {
				t.Fatalf("result records = %d, want 1", len(records))
			}
			if records[0].Level != tt.wantLevel || agenttest.RecordAttrs(records[0])["fault"] != tt.wantFault {
				t.Fatalf("record = %v fault=%q, want %v fault=%q", records[0].Level, agenttest.RecordAttrs(records[0])["fault"], tt.wantLevel, tt.wantFault)
			}
		})
	}
}

// Only the observation whose own write ended the run is recorded. A replay,
// a durable retry that finds the run already terminal and a stale owner all
// observe the run again without applying anything.
func TestRunResultSkipsObservationsThatDidNotEndTheRun(t *testing.T) {
	t.Parallel()
	logger, logs := agenttest.CaptureLogs()

	LogRunResult(context.Background(), logger, sessionruntime.TerminalRun{
		RunID: "run-1", State: string(ledger.StateFailed), ErrorCode: string(apperror.CodeAgentProviderOverloaded),
	})

	if records := logs.RunResults(); len(records) != 0 {
		t.Fatalf("result records = %d, want none for an observation that applied nothing", len(records))
	}
}

// A run the session runtime ended itself, such as one whose admission failed
// or one released at shutdown, reports the cause the runtime held. The run's
// record in session_runs carries only the code, so this record is where that
// cause is found.
func TestRunResultReportsTheSessionRuntimeCause(t *testing.T) {
	t.Parallel()
	logger, logs := agenttest.CaptureLogs()

	LogRunResult(context.Background(), logger, sessionruntime.TerminalRun{
		RunID: "run-1", BotID: "bot-1", SessionID: "session-1",
		State: string(ledger.StateFailed), ErrorCode: string(apperror.CodeRuntimeRunFailed),
		Cause: errors.New("persist user turn failed"), Applied: true,
	})

	records := logs.RunResults()
	if len(records) != 1 {
		t.Fatalf("result records = %d, want 1", len(records))
	}
	if got := agenttest.RecordAttrs(records[0])["error"]; !strings.Contains(got, "persist user turn failed") {
		t.Fatalf("result record error = %q, want the session runtime's cause", got)
	}
}

// A follow-up run starts from the finished run's context; it must not report
// the finished run's cause as its own.
func TestRunResultIgnoresAnotherRunsCause(t *testing.T) {
	t.Parallel()
	logger, logs := agenttest.CaptureLogs()
	ctx := WithRunOutcome(context.Background(), "run-previous", RunOutcome{
		Cause: apperror.Wrap(apperror.CodeAgentProviderOverloaded, errors.New("previous run's failure"), nil),
	})

	LogRunResult(ctx, logger, sessionruntime.TerminalRun{
		RunID: "run-next", State: string(ledger.StateLost), ErrorCode: string(apperror.CodeRuntimeOwnerLeaseExpired),
		Applied: true,
	})

	records := logs.RunResults()
	if len(records) != 1 {
		t.Fatalf("result records = %d, want 1", len(records))
	}
	if got := agenttest.RecordAttrs(records[0])["reason"]; got != string(apperror.CodeRuntimeOwnerLeaseExpired) {
		t.Fatalf("reason = %q, want the run's own code", got)
	}
}
