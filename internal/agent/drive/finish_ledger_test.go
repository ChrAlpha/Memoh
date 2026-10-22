package drive_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/persist"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
)

// Scenario 10 through FinishRun: whatever the cause, only its catalogued
// code reaches the runtime.
//
// Current behavior: a cause without a code finishes as runtime_run_failed; an
// External Agent cause finishes with its code.
func TestFinishRecordsTheCatalogCodeOfTheCause(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status string
		cause  error
		want   [3]string
	}{
		{"plain cause", sessionruntime.RunStatusErrored, errors.New("SECRET adapter crashed"), [3]string{"failed", "runtime_run_failed", ""}},
		{"coded cause", sessionruntime.RunStatusErrored, apperror.Wrap(apperror.CodeWorkspaceUnreachable, errors.New("dial"), nil), [3]string{"failed", "workspace.unreachable", ""}},
		{"external agent cause", sessionruntime.RunStatusErrored, apperror.New(apperror.CodeACPAgentNotConfigured, nil), [3]string{"failed", "acp_agent_not_configured", ""}},
		{"errored without cause", sessionruntime.RunStatusErrored, nil, [3]string{"failed", "runtime_run_failed", ""}},
		{"aborted", sessionruntime.RunStatusAborted, context.Canceled, [3]string{"aborted", "", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runs := sessionledger.New()
			manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
				OwnerID: "owner-finisher", OwnerLeaseTTL: time.Minute, Ledger: runs, Fence: agenttest.NoopFence{},
			})
			t.Cleanup(func() { _ = manager.Close() })
			logger := slog.New(slog.DiscardHandler)
			driver := drive.New(drive.Deps{
				Logger:  logger,
				Runtime: manager,
				Events:  manager,
				Lifecycle: lifecycle.New(lifecycle.Deps{
					Store: &agenttest.RecordingLifecycleStore{}, Runs: agenttest.NoRuns{}, Decisions: agenttest.NoDecisions{}, Logger: logger,
				}),
				Persist: persist.New(persist.Deps{
					Logger:   logger,
					Messages: &agenttest.RecordingMessages{},
					Sessions: &agenttest.Sessions{},
					Bot:      agenttest.BotEnv(logger),
					Runtime:  steerRuntimeUnused{},
				}),
			})
			admission, err := manager.Admit(context.Background(), sessionruntime.AdmitInput{
				BotID: "bot-1", SessionID: "session-1", InvocationID: "invocation-finisher", Payload: []byte(`{}`),
				Execution: sessionruntime.Execution{
					Admission: func(context.Context, sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error) {
						return sessionruntime.RunAdmissionView{}, nil
					},
				},
			})
			if err != nil {
				t.Fatalf("admit: %v", err)
			}
			driver.Finish(context.Background(), drive.AdmittedRun{Handle: admission.Handle, Mode: sessionmode.Chat}, drive.FromOutcome(outcome.RunOutcome{Status: tc.status, Cause: tc.cause}))
			run, err := runs.Get(context.Background(), admission.RunID)
			if err != nil {
				t.Fatalf("load run: %v", err)
			}
			if got := [3]string{string(run.State), run.ErrorCode, run.ErrorMessage}; got != tc.want {
				t.Fatalf("session_runs = %q, want %q", got, tc.want)
			}
		})
	}
}
