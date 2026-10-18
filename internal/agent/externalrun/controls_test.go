package externalrun

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/bots"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
)

type controlQueries struct {
	dbstore.Queries
	row    sqlc.BotSession
	writes int
	stale  bool
}

func (*controlQueries) SupportsTransactions() bool { return true }

func (q *controlQueries) InTx(_ context.Context, fn func(dbstore.Queries) error) error { return fn(q) }

func (*controlQueries) LockBotForSessionWrite(_ context.Context, id pgtype.UUID) (pgtype.UUID, error) {
	return id, nil
}

func (q *controlQueries) LockSessionRuntimeFence(_ context.Context, in sqlc.LockSessionRuntimeFenceParams) (int64, error) {
	if q.stale {
		return 0, pgx.ErrNoRows
	}
	return in.RuntimeFencingToken, nil
}

func (q *controlQueries) GetSessionByID(context.Context, pgtype.UUID) (sqlc.BotSession, error) {
	return q.row, nil
}

func (q *controlQueries) UpdateSessionRuntimeMetadata(_ context.Context, in sqlc.UpdateSessionRuntimeMetadataParams) (sqlc.BotSession, error) {
	if in.FencingToken.Int64 != 1 {
		return sqlc.BotSession{}, errors.New("missing fence")
	}
	q.writes++
	q.row.RuntimeMetadata = in.RuntimeMetadata
	return q.row, nil
}

type controlDriver struct {
	runtimeType  string
	readCalls    int
	commands     []external.Command
	compactCalls int
	mode         string
}

func (d *controlDriver) RuntimeType() string {
	if d.runtimeType != "" {
		return d.runtimeType
	}
	return "codex"
}

func (*controlDriver) Prompt(context.Context, external.PromptInput) (external.PromptResult, error) {
	panic("control must not start chat")
}

func (d *controlDriver) Commands(context.Context, external.PromptInput) ([]external.Command, error) {
	return d.commands, nil
}

func (d *controlDriver) ReadCommand(context.Context, external.PromptInput) (external.CommandResult, error) {
	d.readCalls++
	return external.CommandResult{Data: map[string]any{"native_field": "cached"}, Notice: "last_observed"}, nil
}

func (d *controlDriver) Compact(context.Context, external.PromptInput) (external.CompactionResult, error) {
	d.compactCalls++
	return external.CompactionResult{RuntimeMetadata: map[string]any{"tokens": 12}}, nil
}

func (*controlDriver) Modes(context.Context, external.PromptInput) (external.ModeState, error) {
	return external.ModeState{Supported: true, AvailableModes: []external.Mode{{ID: "choice"}}, CurrentModeID: "choice"}, nil
}

func (d *controlDriver) SetMode(_ context.Context, _ external.PromptInput, id string) (external.ModeState, error) {
	d.mode = id
	return external.ModeState{Supported: true, CurrentModeID: id}, nil
}

func runtimeControlFixture(t *testing.T) (*Host, *controlQueries, *controlDriver, *agenttest.ScriptedAdmitter, turn.RuntimeControlRequest) {
	t.Helper()
	botID, threadID, actorID := uuid.New(), uuid.New(), uuid.New()
	q := &controlQueries{row: sqlc.BotSession{ID: pgtype.UUID{Bytes: threadID, Valid: true}, BotID: pgtype.UUID{Bytes: botID, Valid: true}, CreatedByUserID: pgtype.UUID{Bytes: actorID, Valid: true}, Type: "chat", RuntimeType: "codex", RuntimeMetadata: []byte(`{"keep":"value"}`)}}
	driver := &controlDriver{commands: []external.Command{{Name: "inspect", Kind: external.CommandRead}, {Name: "shrink", Kind: external.CommandOperation}}}
	admitter := agenttest.NewScriptedAdmitter()
	request := turn.RuntimeControlRequest{BotID: botID.String(), ThreadID: threadID.String(), ActorID: actorID.String()}
	host := testHost(t, Deps{
		Sessions:       session.NewService(nil, q, nil),
		BotPermissions: &agenttest.PermissionChecker{Values: map[string]bool{request.BotID + ":" + request.ActorID + ":" + bots.PermissionWorkspaceExec: true}},
		Admitter:       admitter,
	})
	host.drivers = map[string]external.Driver{"codex": driver}
	return host, q, driver, admitter, request
}

func TestRuntimeControlsRejectOtherActorBeforeDispatch(t *testing.T) {
	svc, _, driver, admitter, req := runtimeControlFixture(t)
	req.ActorID = uuid.NewString()
	req.Command = "shrink"
	_, err := svc.ExecuteRuntimeCommand(t.Context(), req)
	if apperror.CodeOf(err) != apperror.CodeRuntimeControlForbidden || driver.compactCalls != 0 || len(admitter.Inputs) != 0 {
		t.Fatalf("unauthorized dispatch: %v", err)
	}
}

func TestRuntimeControlUsesSlotWithoutChatAdmissionAndMergesMetadata(t *testing.T) {
	svc, q, driver, admitter, req := runtimeControlFixture(t)
	req.Command = "shrink"
	if _, err := svc.ExecuteRuntimeCommand(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if driver.compactCalls != 1 || q.writes != 1 || len(admitter.Finishes) != 1 {
		t.Fatal("missing operation lifecycle")
	}
	view, err := admitter.Inputs[0].Execution.Admission(t.Context(), admitter.Finishes[0].Handle)
	if err != nil || view.RequestUserTurn != nil || view.Operation != nil {
		t.Fatalf("control created chat admission: %+v, %v", view, err)
	}
	if admitter.Inputs[0].Execution.ConfigurationOnly {
		t.Fatal("compaction changed its admission presentation")
	}
	var meta map[string]any
	if err := json.Unmarshal(q.row.RuntimeMetadata, &meta); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(meta, map[string]any{"keep": "value", "tokens": float64(12)}) {
		t.Fatalf("metadata: %v", meta)
	}
}

func TestRuntimeModePersistence(t *testing.T) {
	svc, q, driver, _, req := runtimeControlFixture(t)
	req.ModeID = "choice"
	if _, err := svc.SetRuntimeMode(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if driver.mode != "choice" || q.writes != 1 {
		t.Fatal("mode not persisted")
	}
}

type planControlDriver struct{ *controlDriver }

func (*planControlDriver) PlanMode(context.Context, external.PromptInput) (external.ModeState, error) {
	return external.ModeState{Supported: true, AvailableModes: []external.Mode{{ID: "plan"}, {ID: "default"}}}, nil
}

func (d *planControlDriver) SetPlanMode(ctx context.Context, input external.PromptInput, mode string) (external.ModeState, error) {
	state, err := d.PlanMode(ctx, input)
	state.CurrentModeID = mode
	return state, err
}

func TestPlanModePersistsWithoutReplacingPermission(t *testing.T) {
	for _, mode := range []string{"plan", "default"} {
		svc, q, driver, _, req := runtimeControlFixture(t)
		svc.drivers["codex"] = &planControlDriver{driver}
		q.row.RuntimeMetadata = []byte(`{"permission_mode":"yolo","keep":"value"}`)
		req.ModeKind, req.ModeID = "plan", mode
		if _, err := svc.SetRuntimeMode(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		var metadata map[string]any
		if err := json.Unmarshal(q.row.RuntimeMetadata, &metadata); err != nil {
			t.Fatal(err)
		}
		if metadata["permission_mode"] != "yolo" || metadata["collaboration_mode"] != mode || metadata["keep"] != "value" || q.writes != 1 || driver.mode != "" {
			t.Fatalf("mode isolation: %s", q.row.RuntimeMetadata)
		}
	}
}

type failingCompactDriver struct{ *controlDriver }

func (*failingCompactDriver) Compact(context.Context, external.PromptInput) (external.CompactionResult, error) {
	return external.CompactionResult{}, errors.New("compaction unavailable")
}

type openFence struct{}

func (openFence) Activate(context.Context, string, string, int64) error { return nil }

// A control command that fails ends its run through the driver's one terminal
// write, so the run has its lifecycle row without a reconcile pass.
func TestRuntimeControlFailureLeavesALifecycleRow(t *testing.T) {
	svc, _, driver, _, req := runtimeControlFixture(t)
	svc.drivers["codex"] = &failingCompactDriver{driver}
	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID: "owner-control", OwnerLeaseTTL: time.Minute, Ledger: sessionledger.New(), Fence: openFence{},
	})
	t.Cleanup(func() { _ = manager.Close() })
	store := &agenttest.RecordingLifecycleStore{}
	svc.lifecycle = lifecycle.New(lifecycle.Deps{Store: store, Decisions: svc.decider, Logger: svc.logger})
	svc.driver = drive.New(drive.Deps{Logger: svc.logger, Runtime: manager, Lifecycle: svc.lifecycle, Persist: svc.persist})
	svc.admitter = manager
	svc.runtime = manager
	manager.SetTerminalObserver(svc.lifecycle.ReconcileTerminal)

	req.Command = "shrink"
	if _, err := svc.ExecuteRuntimeCommand(t.Context(), req); err == nil {
		t.Fatal("the failing control reported success")
	}
	if len(store.TerminalUpserts) != 1 {
		t.Fatalf("lifecycle rows = %d, want the failed control run's row", len(store.TerminalUpserts))
	}
}

func TestRuntimeControlReportsBusySessionAsTurnBusy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*agenttest.ScriptedAdmitter)
	}{
		{"admission error", func(a *agenttest.ScriptedAdmitter) { a.AdmitErr = sessionruntime.ErrSessionBusy }},
		{"not started", func(a *agenttest.ScriptedAdmitter) { a.Started = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, driver, admitter, req := runtimeControlFixture(t)
			tc.prepare(admitter)
			req.Command = "shrink"
			_, err := svc.ExecuteRuntimeCommand(t.Context(), req)
			if got := apperror.CodeOf(err); got != apperror.CodeSessionBusy {
				t.Fatalf("code = %q (%v), want %q", got, err, apperror.CodeSessionBusy)
			}
			if driver.compactCalls != 0 {
				t.Fatal("busy session dispatched the command")
			}
		})
	}
}

func TestReadCommandAdmissionMatchesRuntimePolicy(t *testing.T) {
	for _, runtime := range []string{"codex", "grok-build"} {
		for _, busy := range []bool{false, true} {
			t.Run(runtime+"/"+strconv.FormatBool(busy), func(t *testing.T) {
				svc, q, driver, admitter, req := runtimeControlFixture(t)
				q.row.RuntimeType = runtime
				driver.runtimeType = runtime
				svc.drivers = map[string]external.Driver{runtime: driver}
				req.Command = "inspect"
				if busy {
					admitter.AdmitErr = sessionruntime.ErrSessionBusy
				}
				result, err := svc.ExecuteRuntimeCommand(t.Context(), req)
				if runtime == "grok-build" && busy {
					if apperror.CodeOf(err) != apperror.CodeSessionBusy || driver.readCalls != 0 {
						t.Fatalf("busy read dispatched: calls=%d err=%v", driver.readCalls, err)
					}
					return
				}
				if err != nil || driver.readCalls != 1 || !reflect.DeepEqual(result.Data, map[string]any{"native_field": "cached"}) {
					t.Fatalf("read result=%+v calls=%d err=%v", result, driver.readCalls, err)
				}
				if runtime == "grok-build" && len(admitter.Finishes) != 1 {
					t.Fatal("exclusive read did not finish admission")
				}
				// The slot keeps the read off a running turn; the read itself is
				// no assistant turn, so clients must not render it as streaming.
				if runtime == "grok-build" && !admitter.Inputs[0].Execution.ConfigurationOnly {
					t.Fatal("exclusive read was admitted as an assistant turn")
				}
				if runtime == "codex" && len(admitter.Inputs) != 0 {
					t.Fatal("cached read unnecessarily took a slot")
				}
			})
		}
	}
}
