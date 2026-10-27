package application

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/runtime/external"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

type managedChildTestStore struct {
	SessionService
	agentSessionService
	child    session.Thread
	order    []string
	claim    string
	released bool
	beginErr error
	listErr  error
}

func (*managedChildTestStore) Get(context.Context, string) (session.Thread, error) {
	return session.Thread{}, pgx.ErrNoRows
}

func (s *managedChildTestStore) ListSubagentsByParent(context.Context, string) ([]session.Thread, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return []session.Thread{s.child}, nil
}

func (s *managedChildTestStore) BeginExternalSubagentExecution(context.Context, session.Thread, string, int) error {
	s.order = append(s.order, "begin")
	return s.beginErr
}

func (s *managedChildTestStore) FinishExternalSubagentExecution(context.Context, string, string) error {
	s.released = true
	return nil
}

func (s *managedChildTestStore) ListExternalExecutionHolds(context.Context, string) ([]session.Thread, error) {
	return []session.Thread{s.child}, nil
}

func (s *managedChildTestStore) ClaimExternalSubagentReap(_ context.Context, _, _, claim string) (bool, error) {
	s.claim = claim
	return true, nil
}

func (s *managedChildTestStore) ResolveExternalSubagentTermination(_ context.Context, _, _, claim string) (bool, error) {
	if claim != s.claim {
		return false, nil
	}
	s.released = true
	return true, nil
}

func (s *managedChildTestStore) MergeRuntimeMetadata(_ context.Context, _, _ string, delta map[string]any) (session.Thread, error) {
	for key, value := range delta {
		s.child.RuntimeMetadata[key] = value
	}
	if _, ok := delta["external_termination_unconfirmed"]; ok {
		s.order = append(s.order, "unknown")
	} else {
		s.order = append(s.order, "anchor")
	}
	return s.child, nil
}

type managedChildTestDriver struct {
	prompt     func(context.Context, external.PromptInput) (external.PromptResult, error)
	reconciled string
	reapErr    error
}

func (*managedChildTestDriver) RuntimeType() string { return "codex" }
func (*managedChildTestDriver) SubagentCapabilities() external.SubagentCapabilities {
	return external.SubagentCapabilities{IsolatedExecution: true, NoNestedDelegation: true, EarlyRuntimeAnchor: true}
}

func (d *managedChildTestDriver) Prompt(ctx context.Context, input external.PromptInput) (external.PromptResult, error) {
	return d.prompt(ctx, input)
}

func (d *managedChildTestDriver) ReconcileSubagent(_ context.Context, input external.PromptInput) error {
	d.reconciled = input.RunID
	return d.reapErr
}

func TestManagedChildPersistsDispatchAndEarlyAnchorBeforeEffects(t *testing.T) {
	store := &managedChildTestStore{child: session.Thread{ID: uuid.NewString(), RuntimeType: "codex", RuntimeMetadata: map[string]any{}}}
	driver := &managedChildTestDriver{prompt: func(ctx context.Context, input external.PromptInput) (external.PromptResult, error) {
		if !input.IsSubagent() || input.CanAskUser() || input.CanRequestUserInput || input.Steering != nil {
			t.Fatal("child escaped its worker policy")
		}
		if err := input.RecordAnchor(ctx, map[string]any{"codex_thread_id": "native-child"}); err != nil {
			return external.PromptResult{}, err
		}
		store.order = append(store.order, "effect")
		return external.PromptResult{Text: "partial result", Execution: external.ExecutionUnknown}, errors.New("private transport failure")
	}}
	wrapper := &managedChildDriver{SubagentDriver: driver, service: &Service{sessionService: store}, child: store.child, store: store}
	result, err := wrapper.Prompt(t.Context(), external.PromptInput{RunID: uuid.NewString(), CanRequestUserInput: true})
	if apperror.CodeOf(err) != apperror.CodeExternalRuntimeTerminationUnconfirmed || result.Text != "partial result" || store.released {
		t.Fatalf("unknown execution lost evidence or capacity: %+v %v", result, err)
	}
	if !reflect.DeepEqual(store.order, []string{"begin", "anchor", "effect", "unknown"}) {
		t.Fatalf("durability order = %v", store.order)
	}
	store.beginErr = session.ErrExternalChildCapacity
	if _, err := wrapper.Prompt(t.Context(), external.PromptInput{}); apperror.CodeOf(err) != apperror.CodeExternalRuntimeBusy {
		t.Fatal(err)
	}
	if len(store.order) != 5 {
		t.Fatal("capacity rejection dispatched work")
	}
}

type childStateQueries struct {
	dbstore.Queries
	run    sqlc.SessionRun
	usages [][]byte
}

func (q *childStateQueries) GetLatestSessionRun(context.Context, pgtype.UUID) (sqlc.SessionRun, error) {
	return q.run, nil
}

func (q *childStateQueries) GetSessionRun(context.Context, pgtype.UUID) (sqlc.SessionRun, error) {
	return q.run, nil
}

func (q *childStateQueries) ListSubagentRunUsage(context.Context, sqlc.ListSubagentRunUsageParams) ([][]byte, error) {
	return q.usages, nil
}

func TestDeletedExternalChildCanBeStoppedByItsExactRun(t *testing.T) {
	bot, parent, child, run := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	store := &managedChildTestStore{child: session.Thread{ID: child, BotID: bot, ParentThreadID: parent, Type: session.TypeSubagent, SessionMode: session.TypeSubagent, RuntimeType: "codex", Metadata: map[string]any{"agent_id": "worker"}, RuntimeMetadata: map[string]any{"external_execution_run": run, "external_child_run": run}}}
	driver := &managedChildTestDriver{}
	queries := &childStateQueries{run: sqlc.SessionRun{RunID: db.ParseUUIDOrEmpty(run), State: "lost"}}
	s := &Service{sessionService: store, queries: queries, externalDrivers: map[string]external.Driver{"codex": driver}, botPermissions: allowWorkspaceExecForBot(bot, "owner")}
	p := &SubagentService{execution: s, sessionService: store, coord: newAgentCoordinator(), logger: slog.New(slog.DiscardHandler)}
	identity := tools.SessionContext{BotID: bot, SessionID: parent, UserID: "owner"}
	if _, err := p.Control(t.Context(), identity, tools.SubagentControlArgs{ID: "worker", Action: "stop", RunID: uuid.NewString()}); apperror.CodeOf(err) != apperror.CodeRuntimeControlRequestInvalid {
		t.Fatal(err)
	}
	if driver.reconciled != "" {
		t.Fatal("stale stop reached driver")
	}
	value, err := p.Control(t.Context(), identity, tools.SubagentControlArgs{ID: "worker", Action: "stop", RunID: run})
	if err != nil {
		t.Fatal(err)
	}
	if driver.reconciled != run || !store.released || value.(map[string]any)["execution_state"] != "stopped" {
		t.Fatalf("deleted hold was unreachable: %v", value)
	}
}

func TestSubagentToolsNeverReturnPrivateStorageDiagnostics(t *testing.T) {
	const marker = "PRIVATE-SUBAGENT-DIAGNOSTIC"
	store := &managedChildTestStore{listErr: errors.New(marker)}
	p := &SubagentService{sessionService: store, logger: slog.New(slog.DiscardHandler)}
	provider := tools.NewSpawnProvider()
	provider.SetController(p)
	definitions, err := provider.Tools(t.Context(), tools.SessionContext{BotID: "bot", SessionID: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range definitions {
		if tool.Name == "list_agents" {
			out, err := tool.Execute(&toolexec.ToolExecContext{Context: t.Context()}, toolexec.ArgumentsFromValue(map[string]any{}))
			raw, _ := json.Marshal(out)
			if err == nil || strings.Contains(err.Error(), marker) || strings.Contains(string(raw), marker) || apperror.CodeOf(err) != apperror.CodeInternal {
				t.Fatalf("private diagnostic escaped: %s %v", raw, err)
			}
		}
	}
}

type childResultMessages struct {
	*recordingMessageService
	rows []messagepkg.Message
}

func (s *childResultMessages) ListLatestBySession(context.Context, string, int32) ([]messagepkg.Message, error) {
	return s.rows, nil
}

func TestChildResultReadbackPreservesKnownAndUnknownUsage(t *testing.T) {
	child := session.Thread{ID: uuid.NewString(), RuntimeType: "codex", RuntimeMetadata: map[string]any{"external_child_run": uuid.NewString()}}
	queries := &childStateQueries{run: sqlc.SessionRun{RunID: db.ParseUUIDOrEmpty(child.RuntimeMetadata["external_child_run"].(string)), TurnID: db.ParseUUIDOrEmpty(uuid.NewString()), State: "completed"}}
	s := &Service{queries: queries, messageService: &childResultMessages{recordingMessageService: &recordingMessageService{}}}
	state, err := s.subagentState(t.Context(), child, true)
	if err != nil {
		t.Fatal(err)
	}
	if state["result"].(map[string]any)["usage"] != nil {
		t.Fatal("missing usage became zero")
	}
	usage := sdk.Usage{InputTokens: 123, OutputTokens: 45, TotalTokens: 168}
	queries.usages = [][]byte{marshalSubagentUsage(&usage)}
	state, err = s.subagentState(t.Context(), child, true)
	if err != nil {
		t.Fatal(err)
	}
	report := state["result"].(map[string]any)
	if got := report["usage"].(*sdk.Usage); got.InputTokens != 123 || got.OutputTokens != 45 || report["usage_scope"] != "runtime_turn" {
		t.Fatalf("usage readback = %v", report)
	}
}
