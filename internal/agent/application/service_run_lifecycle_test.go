package application

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/outcome"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/apperror"
)

func TestTurnRunFinisherCreatesFallbackOnlyAfterFencedTerminalFinish(t *testing.T) {
	abortedCtx, cancel := context.WithCancelCause(context.Background())
	cancel(context.Canceled)
	tests := []struct {
		name        string
		ctx         context.Context
		status      string
		cause       error
		finishErr   error
		wantCreates int
		wantStatus  string
	}{
		{
			name:        "pre-context failure",
			ctx:         context.Background(),
			status:      sessionruntime.RunStatusErrored,
			cause:       apperror.New(apperror.CodeWorkspaceUnreachable, nil),
			wantCreates: 1,
			wantStatus:  lifecycle.StatusFailedProvider,
		},
		{name: "decision pause", ctx: context.Background()},
		{
			name:        "explicit abort",
			ctx:         abortedCtx,
			status:      sessionruntime.RunStatusAborted,
			wantCreates: 1,
			wantStatus:  lifecycle.StatusAborted,
		},
		{
			name:      "lost fence",
			ctx:       context.Background(),
			status:    sessionruntime.RunStatusErrored,
			cause:     errors.New("late failure"),
			finishErr: sessionruntime.ErrRunOwnershipLost,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			admitter := &agenttest.LifecycleAdmitter{FinishErr: tt.finishErr}
			store := &agenttest.RecordingLifecycleStore{}
			service := wireComponents(&Service{sessionRuntime: admitter, lifecycle: lifecycle.New(lifecycle.Deps{Store: store})})
			admission := lifecycleTestAdmission()

			service.turnRunFinisher(tt.ctx, admission, sessionmode.Chat)(outcome.RunOutcome{Status: tt.status, Cause: tt.cause})

			if len(admitter.Finishes) != 1 {
				t.Fatalf("FinishRun calls = %d, want 1", len(admitter.Finishes))
			}
			if tt.name == "pre-context failure" && admitter.Finishes[0].Message != string(apperror.CodeWorkspaceUnreachable) {
				t.Fatalf("stable finish code = %q", admitter.Finishes[0].Message)
			}
			if len(store.Creates) != tt.wantCreates {
				t.Fatalf("lifecycle creates = %d, want %d", len(store.Creates), tt.wantCreates)
			}
			if tt.wantCreates > 0 && store.Creates[0].Status != tt.wantStatus {
				t.Fatalf("lifecycle status = %q, want %q", store.Creates[0].Status, tt.wantStatus)
			}
		})
	}
}

func TestSubagentTerminalPersistsSnapshotAndPreContextFallbackExactlyOnce(t *testing.T) {
	tests := []struct {
		name         string
		terminal     func() tools.SubagentTerminal
		wantStatus   string
		wantFinish   string
		wantSnapshot contextfrag.LifecycleSnapshot
	}{
		{
			name: "final snapshot",
			terminal: func() tools.SubagentTerminal {
				snapshot, ok := agenttest.LifecycleRunConfig().ContextLifecycle.Snapshot()
				if !ok {
					t.Fatal("test lifecycle snapshot is unavailable")
				}
				return tools.SubagentTerminal{ContextLifecycle: &snapshot}
			},
			wantStatus:   lifecycle.StatusCompleted,
			wantFinish:   sessionruntime.RunStatusCompleted,
			wantSnapshot: contextfrag.LifecycleSnapshot{Version: contextfrag.LifecycleSnapshotVersion},
		},
		{
			name: "failure before context",
			terminal: func() tools.SubagentTerminal {
				return tools.SubagentTerminal{Cause: apperror.New(apperror.CodeWorkspaceUnreachable, nil)}
			},
			wantStatus:   lifecycle.StatusFailedProvider,
			wantFinish:   sessionruntime.RunStatusErrored,
			wantSnapshot: contextfrag.LifecycleSnapshot{Version: contextfrag.LifecycleSnapshotVersion},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			admission := lifecycleTestAdmission()
			admitter := &agenttest.LifecycleAdmitter{Admission: admission}
			store := &agenttest.RecordingLifecycleStore{}
			service := wireComponents(&Service{sessionRuntime: admitter, lifecycle: lifecycle.New(lifecycle.Deps{Store: store})})

			_, subagentAdmission, terminal, err := service.AdmitSubagentRun(
				context.Background(),
				agenttest.LifecycleBotID,
				agenttest.LifecycleSessionID,
				"subagent:test",
				[]byte(`{"message":"work"}`),
			)
			if err != nil {
				t.Fatalf("AdmitSubagentRun() error = %v", err)
			}
			if subagentAdmission.RunID != agenttest.LifecycleRunID {
				t.Fatalf("run ID = %q, want %q", subagentAdmission.RunID, agenttest.LifecycleRunID)
			}
			terminal(tt.terminal())
			terminal(tools.SubagentTerminal{Cause: errors.New("late duplicate")})

			if len(store.Creates) != 1 || store.Creates[0].Status != tt.wantStatus {
				t.Fatalf("lifecycle creates = %#v, want one %s", store.Creates, tt.wantStatus)
			}
			var snapshot contextfrag.LifecycleSnapshot
			if err := json.Unmarshal(store.Creates[0].Snapshot, &snapshot); err != nil {
				t.Fatalf("decode snapshot: %v", err)
			}
			if snapshot.Version != tt.wantSnapshot.Version {
				t.Fatalf("snapshot = %#v, want version %d", snapshot, tt.wantSnapshot.Version)
			}
			if len(admitter.Finishes) != 1 || admitter.Finishes[0].Status != tt.wantFinish {
				t.Fatalf("runtime finishes = %#v, want one %s", admitter.Finishes, tt.wantFinish)
			}
		})
	}
}

func TestCanceledSubagentUsesOwningContextToPersistAborted(t *testing.T) {
	admitter := &agenttest.LifecycleAdmitter{Admission: lifecycleTestAdmission()}
	store := &agenttest.RecordingLifecycleStore{}
	service := wireComponents(&Service{sessionRuntime: admitter, lifecycle: lifecycle.New(lifecycle.Deps{Store: store})})
	runCtx, _, terminal, err := service.AdmitSubagentRun(
		context.Background(),
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
		"subagent:abort",
		[]byte(`{"message":"work"}`),
	)
	if err != nil {
		t.Fatalf("AdmitSubagentRun() error = %v", err)
	}
	snapshot, ok := agenttest.LifecycleRunConfig().ContextLifecycle.Snapshot()
	if !ok {
		t.Fatal("test lifecycle snapshot is unavailable")
	}

	admitter.Cancel()
	<-runCtx.Done()
	terminal(tools.SubagentTerminal{Cause: context.Canceled, ContextLifecycle: &snapshot})

	if len(store.Creates) != 1 || store.Creates[0].Status != lifecycle.StatusAborted {
		t.Fatalf("lifecycle creates = %#v, want one aborted row", store.Creates)
	}
	if len(admitter.Finishes) != 1 || admitter.Finishes[0].Status != sessionruntime.RunStatusAborted {
		t.Fatalf("runtime finishes = %#v, want one aborted finish", admitter.Finishes)
	}
}

func lifecycleTestAdmission() sessionruntime.Admission {
	return sessionruntime.Admission{
		RunID:   agenttest.LifecycleRunID,
		Started: true,
		Handle: sessionruntime.RunHandle{
			RunID:        agenttest.LifecycleRunID,
			BotID:        agenttest.LifecycleBotID,
			SessionID:    agenttest.LifecycleSessionID,
			FencingToken: 1,
		},
	}
}
