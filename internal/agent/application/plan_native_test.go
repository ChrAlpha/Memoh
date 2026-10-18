package application

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/decide"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/externalrun"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/contextview"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/runtimefence"
	"github.com/felinics/memoh/internal/schedule"
	sessiontest "github.com/felinics/memoh/internal/testutil/sessionruntime"
	"github.com/felinics/memoh/internal/workspace"
)

// These tests pin the native run plan's tail as the three entries build it:
// chat (chatSetup), a scheduled fire (scheduleSetup) and the run an answered
// decision resumes (the approval and ask_user continuations). They assert only
// through the adapters below, drive.RunPlan, the plan's executor and the
// native.RunConfig the agent receives, so the tail can move without the
// assertions changing.

// ---------------------------------------------------------------- ADAPTER
//
// Everything below this banner up to the closing banner is the only place the
// tests touch code that the plan tail is built from.

// adaptChat resolves a chat entry's native plan (plan.ChatSetup).
func adaptChat(ctx context.Context, s *Service, req runreq.ChatRequest, spec drive.RunSpec) (drive.RunPlan, error) {
	return s.plan.ChatSetup(ctx, req, spec)
}

// adaptSchedule resolves a scheduled fire's native plan through the schedule
// entry's own setup (plan.ScheduleSetup).
func adaptSchedule(ctx context.Context, s *Service, spec drive.RunSpec, payload schedule.TriggerPayload, req runreq.ChatRequest) (drive.RunPlan, error) {
	setup := s.plan.ScheduleSetup(spec, payload, req.BotID, "", externalrun.Dispatch{})
	return setup(ctx, req)
}

type decisionKind string

const (
	kindApproval  decisionKind = "tool approval"
	kindUserInput decisionKind = "user input"
)

var decisionKinds = []decisionKind{kindUserInput, kindApproval}

// adaptContinueResume resolves the native run that resumes after the answer of
// the given kind, with the same fixed decision as the existing continuation
// tests. The call carries its result already, so planning it stores that
// result and resumes (plan.Continue).
func adaptContinueResume(ctx context.Context, s *Service, kind decisionKind, req runreq.ChatRequest, live bool) (drive.RunPlan, error) {
	call := decide.DecidedCall{
		Verdict: decide.VerdictAnswered, BotID: agenttest.LifecycleBotID, ScopeBotID: agenttest.LifecycleBotID,
		SessionID: agenttest.LifecycleSessionID, SourcePlatform: "web",
	}
	switch kind {
	case kindUserInput:
		call.Kind, call.DecisionID, call.ToolCallID, call.ToolName = decide.CallUserInput, "user-input", "ask-user-call", "ask_user"
	default:
		call.Kind, call.DecisionID, call.ToolCallID, call.ToolName = decide.CallToolApproval, "tool-approval", "approved-tool-call", "container_exec"
	}
	call.Result = sdk.ToolResultPart{ToolCallID: call.ToolCallID, ToolName: call.ToolName, Result: sdk.TextOutput("result")}
	return s.plan.Continue(ctx, call, req, live)
}

// adaptApprovalContinuation builds the continuation of a committed approval
// (toolApprovalContinuation).
func adaptApprovalContinuation(ctx context.Context, s *Service, committed decide.CommittedToolApprovalResponse, live bool) decisionContinuation {
	return s.toolApprovalContinuation(ctx, committed, live)
}

// adaptUserInputContinuation builds the continuation of a committed answer
// (userInputContinuation).
func adaptUserInputContinuation(ctx context.Context, s *Service, committed decide.CommittedUserInputResponse, live bool) decisionContinuation {
	return s.userInputContinuation(ctx, committed, live)
}

// adaptBindSessionManager makes the session manager the one the plan tail
// reads step offsets from and the history writer steers through
// (bindRuntime, then the history writer and the planner follow it).
func adaptBindSessionManager(s *Service, manager *sessionruntime.Manager) {
	bindRuntime(s, manager)
	rewireRun(s)
}

// ---------------------------------------------------------------- END ADAPTER

// planCapture records the RunConfig the agent receives at Stream time.
type planCapture struct {
	mu   sync.Mutex
	cfgs []native.RunConfig
}

func (c *planCapture) apply(ctx context.Context, cfg native.RunConfig) native.RunConfig {
	c.mu.Lock()
	c.cfgs = append(c.cfgs, cfg)
	c.mu.Unlock()
	return contextview.ProviderRunConfigApplier(slog.New(slog.DiscardHandler))(ctx, cfg)
}

func (c *planCapture) first(t *testing.T) native.RunConfig {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cfgs) == 0 {
		t.Fatal("agent.Stream was never reached; no RunConfig captured")
	}
	return c.cfgs[0]
}

// installCapture replaces the service's agent with one that records the
// RunConfig it streams. Call it before rewiring the components, which read the
// agent for their output limits.
func installCapture(s *Service, limits native.Limits, providers ...tools.ToolProvider) *planCapture {
	capture := &planCapture{}
	agent := native.New(native.Deps{
		Logger:             slog.New(slog.DiscardHandler),
		ContextViewApplier: capture.apply,
		Limits:             limits,
	})
	if len(providers) > 0 {
		agent.SetToolProviders(providers)
	}
	s.agent = agent
	return capture
}

// stepMessages is a message service that also persists agent steps, which is
// what lets persist.Writer.NewStepCommitter return a committer.
type stepMessages struct {
	*agenttest.RecordingMessages
	mu    sync.Mutex
	steps []messagepkg.AgentStep
}

func (m *stepMessages) PersistAgentStep(_ context.Context, step messagepkg.AgentStep) ([]messagepkg.Message, error) {
	m.mu.Lock()
	m.steps = append(m.steps, step)
	m.mu.Unlock()
	out := make([]messagepkg.Message, 0, len(step.Messages))
	for _, input := range step.Messages {
		out = append(out, messagepkg.Message{ID: "step-message", SessionID: input.SessionID, Role: input.Role, Content: input.Content})
	}
	return out, nil
}

func (m *stepMessages) stepCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.steps)
}

func withStepMessages(s *Service) *stepMessages {
	messages := &stepMessages{RecordingMessages: &agenttest.RecordingMessages{}}
	s.messageService = messages
	return messages
}

func fencedContext(ctx context.Context, token int64) context.Context {
	return runtimefence.WithContext(ctx, runtimefence.Fence{
		BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID, Token: token,
	})
}

func execute(ctx context.Context, plan drive.RunPlan) drive.ExecEnding {
	return plan.Exec.Execute(ctx, func(native.StreamEvent) {})
}

func chatPlanRequest() runreq.ChatRequest {
	return runreq.ChatRequest{
		RunID: agenttest.LifecycleRunID, BotID: agenttest.LifecycleBotID, ChatID: agenttest.LifecycleBotID,
		ThreadID: agenttest.LifecycleSessionID, Query: directLifecyclePrompt, CurrentChannel: "web",
		UserMessagePersisted: true,
	}
}

// newChatPlanService is the chat fixture's service with a capturing agent and,
// when withSteps is set, a message service that persists agent steps.
func newChatPlanService(t *testing.T, withSteps bool) (*Service, *planCapture, *stepMessages) {
	t.Helper()
	service := newChatTurnFixture(t, directLifecycleModelSuccess).fixture.service
	capture := installCapture(service, native.Limits{})
	var steps *stepMessages
	if withSteps {
		steps = withStepMessages(service)
	}
	rewireComponents(service)
	return service, capture, steps
}

func schedulePlanSpec() drive.RunSpec {
	position := int64(1)
	return drive.RunSpec{
		Run: drive.AdmittedRun{TurnID: "schedule-turn", TurnPosition: &position},
		Req: runreq.ChatRequest{BotID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID, RunID: agenttest.LifecycleRunID},
		// The schedule entry is unattended.
		Unattended: true,
	}
}

func newSchedulePlanService(t *testing.T, withSteps bool) (*Service, *planCapture, *stepMessages) {
	t.Helper()
	service := newDirectLifecycleFixture(t, directLifecycleModelSuccess).service
	capture := installCapture(service, native.Limits{})
	var steps *stepMessages
	if withSteps {
		steps = withStepMessages(service)
	}
	rewireComponents(service)
	return service, capture, steps
}

func scheduleFire(ctx context.Context, t *testing.T, s *Service) drive.RunPlan {
	t.Helper()
	spec := schedulePlanSpec()
	plan, err := adaptSchedule(ctx, s, spec, driveSchedulePayload("schedule-owner"), spec.Req)
	if err != nil {
		t.Fatalf("schedule plan error = %v", err)
	}
	return plan
}

func newContinuationPlanService(t *testing.T, withSteps bool) (*Service, *planCapture, *stepMessages) {
	return newSchedulePlanService(t, withSteps)
}

// ---- A. interaction flags -------------------------------------------------

func TestPlanNativeChatOffersLiveToolStreamAndUserInput(t *testing.T) {
	t.Parallel()
	service, capture, _ := newChatPlanService(t, false)
	plan, err := adaptChat(context.Background(), service, chatPlanRequest(), drive.RunSpec{})
	if err != nil {
		t.Fatalf("chat plan error = %v", err)
	}
	execute(context.Background(), plan)
	cfg := capture.first(t)
	if !cfg.LiveToolStream || !cfg.CanRequestUserInput {
		t.Fatalf("chat LiveToolStream=%v CanRequestUserInput=%v, want both true", cfg.LiveToolStream, cfg.CanRequestUserInput)
	}
}

func TestPlanNativeScheduleOffersNeitherLiveToolStreamNorUserInput(t *testing.T) {
	t.Parallel()
	service, capture, _ := newSchedulePlanService(t, false)
	plan := scheduleFire(context.Background(), t, service)
	execute(context.Background(), plan)
	cfg := capture.first(t)
	if cfg.LiveToolStream || cfg.CanRequestUserInput {
		t.Fatalf("schedule LiveToolStream=%v CanRequestUserInput=%v, want both false", cfg.LiveToolStream, cfg.CanRequestUserInput)
	}
	if cfg.SessionType != "schedule" {
		t.Fatalf("schedule SessionType = %q, want schedule", cfg.SessionType)
	}
	if cfg.Identity.ChannelIdentityID != "schedule-owner" || cfg.ContextScope.ChannelIdentityID != "schedule-owner" {
		t.Fatalf("schedule identity = %q / scope %q, want the payload owner", cfg.Identity.ChannelIdentityID, cfg.ContextScope.ChannelIdentityID)
	}
	if !strings.Contains(cfg.Query, directLifecyclePrompt) {
		t.Fatalf("schedule Query = %q, want the generated schedule prompt carrying the command", cfg.Query)
	}
}

func TestPlanNativeContinuationInteractionFlagsFollowLive(t *testing.T) {
	t.Parallel()
	for _, kind := range decisionKinds {
		for _, live := range []bool{true, false} {
			t.Run(string(kind), func(t *testing.T) {
				t.Parallel()
				service, capture, _ := newContinuationPlanService(t, false)
				plan, err := adaptContinueResume(context.Background(), service, kind, continuationSessionRequest(), live)
				if err != nil {
					t.Fatalf("continuation plan error = %v", err)
				}
				execute(context.Background(), plan)
				cfg := capture.first(t)
				if cfg.LiveToolStream != live || cfg.CanRequestUserInput != live {
					t.Fatalf("live=%v: LiveToolStream=%v CanRequestUserInput=%v, want both %v",
						live, cfg.LiveToolStream, cfg.CanRequestUserInput, live)
				}
			})
		}
	}
}

// ---- B. step commits and reasoning timing ---------------------------------

func TestPlanNativeChatCommitsStepsAndTimesReasoning(t *testing.T) {
	t.Parallel()
	service, capture, steps := newChatPlanService(t, true)
	ctx := fencedContext(context.Background(), 7)
	plan, err := adaptChat(ctx, service, chatPlanRequest(), drive.RunSpec{})
	if err != nil {
		t.Fatalf("chat plan error = %v", err)
	}
	execute(ctx, plan)
	if steps.stepCalls() == 0 {
		t.Fatal("PersistAgentStep was never called; the chat plan has no step committer")
	}
	cfg := capture.first(t)
	if cfg.OnProviderStreamEventObserved == nil || cfg.OnStepCommitted == nil {
		t.Fatalf("chat cfg observer=%v onStepCommitted=%v, want both configured",
			cfg.OnProviderStreamEventObserved != nil, cfg.OnStepCommitted != nil)
	}
}

func TestPlanNativeScheduleCommitsStepsAndTimesReasoning(t *testing.T) {
	t.Parallel()
	service, capture, steps := newSchedulePlanService(t, true)
	ctx := fencedContext(context.Background(), 7)
	plan := scheduleFire(ctx, t, service)
	execute(ctx, plan)
	if steps.stepCalls() == 0 {
		t.Fatal("PersistAgentStep was never called; the schedule plan has no step committer")
	}
	cfg := capture.first(t)
	if cfg.OnProviderStreamEventObserved == nil || cfg.OnStepCommitted == nil {
		t.Fatalf("schedule cfg observer=%v onStepCommitted=%v, want both configured",
			cfg.OnProviderStreamEventObserved != nil, cfg.OnStepCommitted != nil)
	}
}

func TestPlanNativeUnattendedChatHasNoCommitterAndNoReasoningTiming(t *testing.T) {
	t.Parallel()
	service, capture, steps := newChatPlanService(t, true)
	ctx := fencedContext(context.Background(), 7)
	plan, err := adaptChat(ctx, service, chatPlanRequest(), drive.RunSpec{Unattended: true})
	if err != nil {
		t.Fatalf("chat plan error = %v", err)
	}
	execute(ctx, plan)
	if calls := steps.stepCalls(); calls != 0 {
		t.Fatalf("PersistAgentStep calls = %d, want 0 for an unattended chat run", calls)
	}
	cfg := capture.first(t)
	if cfg.OnProviderStreamEventObserved != nil || cfg.OnStepCommitted != nil || cfg.OnStepInterrupted != nil {
		t.Fatalf("unattended chat cfg observer=%v committed=%v interrupted=%v, want all nil",
			cfg.OnProviderStreamEventObserved != nil, cfg.OnStepCommitted != nil, cfg.OnStepInterrupted != nil)
	}
}

func TestPlanNativeContinuationWithIncompleteHandleTimesReasoningWithoutCommitter(t *testing.T) {
	t.Parallel()
	for _, kind := range decisionKinds {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			service, capture, steps := newContinuationPlanService(t, true)
			ctx := fencedContext(context.Background(), 7)
			plan, err := adaptContinueResume(ctx, service, kind, continuationSessionRequest(), true)
			if err != nil {
				t.Fatalf("continuation plan error = %v", err)
			}
			execute(ctx, plan)
			if calls := steps.stepCalls(); calls != 0 {
				t.Fatalf("PersistAgentStep calls = %d, want 0 without a complete run handle", calls)
			}
			cfg := capture.first(t)
			if cfg.OnProviderStreamEventObserved == nil {
				t.Fatal("reasoning timing is not configured on the continuation cfg")
			}
			if plan.Req.StepIndexOffset != 0 || plan.Req.QueueSteerEnabled {
				t.Fatalf("plan.Req StepIndexOffset=%d QueueSteerEnabled=%v, want 0/false",
					plan.Req.StepIndexOffset, plan.Req.QueueSteerEnabled)
			}
			if cfg.StepIndexOffset != 0 {
				t.Fatalf("cfg.StepIndexOffset = %d, want 0", cfg.StepIndexOffset)
			}
		})
	}
}

// ---- C. continuation step offset and queue binding ------------------------

const planRunOwner = "owner-plan-native"

// startPlanRun admits a run on the lifecycle session in a real session
// manager, bound as the service's runtime, and advances the run's step cursor
// by finishing step stepNumber.
func startPlanRun(t *testing.T, service *Service, stepNumber int) sessionruntime.RunHandle {
	t.Helper()
	manager := sessiontest.New(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID:       planRunOwner,
		StateTTL:      time.Minute,
		OwnerLeaseTTL: time.Minute,
		CommandAckTTL: time.Second,
	})
	t.Cleanup(func() { _ = manager.Close() })
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("start session manager: %v", err)
	}
	adaptBindSessionManager(service, manager)
	handle, err := sessiontest.Start(context.Background(), manager,
		agenttest.LifecycleBotID, agenttest.LifecycleSessionID, agenttest.LifecycleRunID,
		make(chan struct{}, 1), func() {}, nil,
	)
	if err != nil {
		t.Fatalf("admit run: %v", err)
	}
	if handle.RunID == "" || handle.OwnerID == "" || handle.FencingToken <= 0 {
		t.Fatalf("admitted handle is not complete: %#v", handle)
	}
	if stepNumber >= 0 {
		if _, err := manager.HandleAgentEvent(context.Background(), handle, native.StreamEvent{
			Type: native.EventStepEnd, StepNumber: stepNumber,
		}); err != nil {
			t.Fatalf("mark step consumed: %v", err)
		}
	}
	return handle
}

func TestPlanNativeContinuationTakesStepOffsetFromSessionRuntimeAndBindsQueue(t *testing.T) {
	t.Parallel()
	for _, kind := range decisionKinds {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			service, capture, steps := newContinuationPlanService(t, true)
			handle := startPlanRun(t, service, 2)
			req := continuationSessionRequest()
			req.RunHandle = handle
			ctx := fencedContext(context.Background(), handle.FencingToken)

			plan, err := adaptContinueResume(ctx, service, kind, req, true)
			if err != nil {
				t.Fatalf("continuation plan error = %v", err)
			}
			if plan.Req.StepIndexOffset != 3 {
				t.Fatalf("plan.Req.StepIndexOffset = %d, want 3 (step 2 consumed)", plan.Req.StepIndexOffset)
			}
			if !plan.Req.QueueSteerEnabled {
				t.Fatal("plan.Req.QueueSteerEnabled = false, want true")
			}
			execute(ctx, plan)
			cfg := capture.first(t)
			if cfg.StepIndexOffset != 3 {
				t.Fatalf("cfg.StepIndexOffset = %d, want 3", cfg.StepIndexOffset)
			}
			if cfg.PendingSteer == nil || cfg.OnSteer == nil {
				t.Fatalf("cfg PendingSteer=%v OnSteer=%v, want the committer bound",
					cfg.PendingSteer != nil, cfg.OnSteer != nil)
			}
			if cfg.OnProviderStreamEventObserved == nil {
				t.Fatal("reasoning timing is not configured")
			}
			if steps.stepCalls() == 0 {
				t.Fatal("the resumed step was not committed through the live queue committer")
			}
		})
	}
}

func TestPlanNativeContinuationWithLostRunReportsOwnershipLost(t *testing.T) {
	t.Parallel()
	for _, kind := range decisionKinds {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			service, _, _ := newContinuationPlanService(t, true)
			manager := sessiontest.New(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
				OwnerID: planRunOwner, StateTTL: time.Minute, OwnerLeaseTTL: time.Minute,
			})
			t.Cleanup(func() { _ = manager.Close() })
			adaptBindSessionManager(service, manager)
			req := continuationSessionRequest()
			req.RunHandle = sessionruntime.RunHandle{
				BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID,
				RunID: agenttest.LifecycleRunID, OwnerID: planRunOwner, FencingToken: 5,
			}
			ctx := fencedContext(context.Background(), 5)
			_, err := adaptContinueResume(ctx, service, kind, req, true)
			if !errors.Is(err, sessionruntime.ErrRunOwnershipLost) {
				t.Fatalf("continuation error = %v, want ErrRunOwnershipLost", err)
			}
		})
	}
}

func TestPlanNativeContinuationWithoutStepPersisterFailsAtSetup(t *testing.T) {
	t.Parallel()
	for _, kind := range decisionKinds {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			// The message service does not persist agent steps, so a complete
			// handle cannot get a committer.
			service, _, _ := newContinuationPlanService(t, false)
			handle := startPlanRun(t, service, -1)
			req := continuationSessionRequest()
			req.RunHandle = handle
			ctx := fencedContext(context.Background(), handle.FencingToken)
			_, err := adaptContinueResume(ctx, service, kind, req, true)
			const want = "live queue step committer is unavailable for decision continuation"
			if err == nil || err.Error() != want {
				t.Fatalf("continuation error = %v, want %q", err, want)
			}
		})
	}
}

// ---- D. chat replacement --------------------------------------------------

func TestPlanNativeChatReplacementPostPersistRunsOnceWithoutCommitter(t *testing.T) {
	t.Parallel()
	service, _, _ := newChatPlanService(t, false)
	var calls int
	var received []messagepkg.Message
	spec := drive.RunSpec{Replacement: &drive.TurnReplacement{
		PostPersist: func(_ context.Context, persisted []messagepkg.Message) error {
			calls++
			received = persisted
			return nil
		},
	}}
	plan, err := adaptChat(context.Background(), service, chatPlanRequest(), spec)
	if err != nil {
		t.Fatalf("chat plan error = %v", err)
	}
	ending := execute(context.Background(), plan)
	if ending.Terminal == nil {
		t.Fatalf("the run produced no terminal event; cause = %v", ending.Cause)
	}
	if _, err := ending.Commit(context.Background(), nil, false); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("PostPersist calls = %d, want 1", calls)
	}
	if len(received) == 0 {
		t.Fatal("PostPersist received no persisted messages")
	}
}

// ---- E. continuation chain ------------------------------------------------

const (
	planApprovalToolName = "plan_edit"
	planUIDiff           = "--- a/file\n+++ b/file\n@@ -1 +1 @@\n-old\n+new\n"
)

// planTool is a tool whose output carries a UI-only payload next to its
// model-visible result, and which records the arguments it was called with.
type planTool struct {
	mu     sync.Mutex
	inputs []map[string]any
}

func (p *planTool) Tools(context.Context, tools.SessionContext) ([]toolexec.Tool, error) {
	return []toolexec.Tool{{
		Name: planApprovalToolName,
		Execute: func(_ *toolexec.ToolExecContext, input sdk.ToolArguments) (sdk.ToolOutput, error) {
			var args map[string]any
			if err := input.Unmarshal(&args); err != nil {
				return sdk.ToolOutput{}, err
			}
			p.mu.Lock()
			p.inputs = append(p.inputs, args)
			p.mu.Unlock()
			return toolexec.OutputFromValue(map[string]any{
				"status":                  "edited",
				tools.UIOutputMetadataKey: map[string]any{"diff": planUIDiff},
			}), nil
		},
	}}, nil
}

func (p *planTool) calls() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.inputs...)
}

// decisionQueries answers the fenced transaction an approved tool's execution
// checkpoints in, and records the checkpoint.
type decisionQueries struct {
	dbstore.Queries
	mu      sync.Mutex
	params  []sqlc.MarkSessionRunDecisionExecutingParams
	lineage []pgtype.UUID
	count   int64
}

func (q *decisionQueries) InTx(_ context.Context, fn func(dbstore.Queries) error) error { return fn(q) }

func (*decisionQueries) SupportsTransactions() bool { return true }

func (*decisionQueries) LockBotForSessionWrite(_ context.Context, id pgtype.UUID) (pgtype.UUID, error) {
	return id, nil
}

func (*decisionQueries) LockSessionRuntimeFence(context.Context, sqlc.LockSessionRuntimeFenceParams) (int64, error) {
	return 1, nil
}

func (q *decisionQueries) MarkSessionRunDecisionExecuting(_ context.Context, arg sqlc.MarkSessionRunDecisionExecutingParams) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.params = append(q.params, arg)
	return q.count, nil
}

func (q *decisionQueries) ListCompactionArtifactLineageBySession(ctx context.Context, sessionID pgtype.UUID) ([]sqlc.BotHistoryMessageCompact, error) {
	q.mu.Lock()
	q.lineage = append(q.lineage, sessionID)
	q.mu.Unlock()
	return q.Queries.ListCompactionArtifactLineageBySession(ctx, sessionID)
}

func (q *decisionQueries) lineageSessions() []pgtype.UUID {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]pgtype.UUID(nil), q.lineage...)
}

func (q *decisionQueries) recorded() []sqlc.MarkSessionRunDecisionExecutingParams {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]sqlc.MarkSessionRunDecisionExecutingParams(nil), q.params...)
}

// decisionHistory is a session history that also names the request message of
// the turn a decision parked, which is what a continuation resumes from.
type decisionHistory struct {
	*driveHistoryMessages
	requestID string
	err       error
}

func (m *decisionHistory) GetVisibleHistoryTurnRequestMessageIDByTurn(context.Context, string, string) (string, error) {
	return m.requestID, m.err
}

const planWorkspaceTargetID = "plan-workspace-target"

// planTargets resolves the workspace target named by the argument or pinned on the context to a container target, so a
// continuation's resolved run config carries one.
type planTargets struct{}

func (planTargets) ResolveWorkspaceTarget(ctx context.Context, _, targetID string) (workspace.ResolvedWorkspaceTarget, error) {
	if targetID == "" {
		targetID = workspace.WorkspaceTargetFromContext(ctx)
	}
	if targetID == "" {
		return workspace.ResolvedWorkspaceTarget{}, nil
	}
	return workspace.ResolvedWorkspaceTarget{TargetID: targetID, Kind: "container", Name: "plan workspace"}, nil
}

const planDecisionTurnID = "decision-turn"

func planDecisionHandle() sessionruntime.RunHandle {
	return sessionruntime.RunHandle{TurnID: planDecisionTurnID}
}

type decisionHarness struct {
	service *Service
	history *decisionHistory
	capture *planCapture
	tool    *planTool
	queries *decisionQueries
}

// newDecisionHarness is a service whose latest turn called toolName and whose
// agent offers planTool, with the output limits the limit tests need.
func newDecisionHarness(t *testing.T, toolName string) decisionHarness {
	t.Helper()
	service := newDirectLifecycleFixture(t, directLifecycleModelSuccess).service
	tool := &planTool{}
	capture := installCapture(service, native.Limits{ToolOutputMaxBytes: 512, ToolOutputMaxLines: 80}, tool)
	history := &decisionHistory{driveHistoryMessages: driveDecisionHistory(t, toolName, sdk.TextOutput("{}")), requestID: driveRequestMessageID}
	service.messageService = history
	queries := &decisionQueries{Queries: service.queries, count: 1}
	service.queries = queries
	rewireComponents(service)
	useRunconfig(service, func(d *runconfig.Deps) { d.WorkspaceTargets = planTargets{} })
	rewirePlan(service)
	return decisionHarness{service: service, history: history, capture: capture, tool: tool, queries: queries}
}

func planApproval(status string) (toolapproval.Request, decide.ToolApprovalResponseInput) {
	approval := toolapproval.Request{
		ID: "tool-approval", BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID,
		ToolCallID: driveToolCallID, ToolName: planApprovalToolName, Status: status,
		ToolInput:      map[string]any{"path": "/work/file.txt", "new": "new"},
		SourcePlatform: "telegram", ReplyTarget: "im-chat-1", ConversationType: "private",
		WorkspaceTargetID: planWorkspaceTargetID,
	}
	input := decide.ToolApprovalResponseInput{BotID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID}
	return approval, input
}

func approvalContinuation(ctx context.Context, h decisionHarness, approval toolapproval.Request, input decide.ToolApprovalResponseInput, live bool) decisionContinuation {
	committed := decide.NewCommittedToolApproval(approval, input, false).ForRun(agenttest.LifecycleRunID, planDecisionHandle())
	return adaptApprovalContinuation(ctx, h.service, committed, live)
}

func planAnswer(platform, replyTarget string) userinput.Request {
	return userinput.Request{
		ID: "user-input", BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID,
		ToolCallID: driveToolCallID, ToolName: "ask_user", Status: userinput.StatusSubmitted,
		SourcePlatform: platform, ReplyTarget: replyTarget, ConversationType: "private",
		WorkspaceTargetID: planWorkspaceTargetID,
		Result:            map[string]any{"answers": []any{map[string]any{"text": "blue"}}},
	}
}

func userInputContinuation(ctx context.Context, h decisionHarness, answered userinput.Request, live bool) decisionContinuation {
	input := decide.UserInputResponseInput{BotID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID}
	committed := decide.NewCommittedUserInput(answered, input).ForRun(agenttest.LifecycleRunID, planDecisionHandle())
	return adaptUserInputContinuation(ctx, h.service, committed, live)
}

func toolRows(h decisionHarness) []messagepkg.PersistInput {
	var rows []messagepkg.PersistInput
	for _, input := range h.history.Persisted {
		if input.Role == "tool" {
			rows = append(rows, input)
		}
	}
	return rows
}

func TestPlanNativeApprovedApprovalExecutesToolAndStoresUIOnlyDataOnRowMetadata(t *testing.T) {
	t.Parallel()
	h := newDecisionHarness(t, planApprovalToolName)
	approval, input := planApproval(toolapproval.StatusApproved)
	cont := approvalContinuation(context.Background(), h, approval, input, true)
	plan, err := cont.Setup(context.Background(), cont.Req)
	if err != nil {
		t.Fatalf("approval setup error = %v", err)
	}
	calls := h.tool.calls()
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], approval.ToolInput) {
		t.Fatalf("tool calls = %#v, want one call with %#v", calls, approval.ToolInput)
	}
	rows := toolRows(h)
	if len(rows) != 1 {
		t.Fatalf("persisted tool rows = %d, want 1", len(rows))
	}
	content := string(rows[0].Content)
	if !strings.Contains(content, "edited") {
		t.Fatalf("tool row content = %s, want the tool's result", content)
	}
	if strings.Contains(content, "diff") || strings.Contains(content, tools.UIOutputMetadataKey) || strings.Contains(content, "+new") {
		t.Fatalf("tool row content = %s leaks the UI-only payload to the model", content)
	}
	if got := rows[0].Metadata["diff"]; got != planUIDiff {
		t.Fatalf("tool row metadata diff = %#v, want the UI diff", got)
	}
	if strings.Contains(content, `"is_error":true`) || strings.Contains(content, `"isError":true`) {
		t.Fatalf("approved tool row is marked as an error: %s", content)
	}
	// The resumed run is the approval's.
	if plan.Req.SkipMemoryExtraction {
		t.Fatal("approval continuation sets SkipMemoryExtraction, want false")
	}
}

func TestPlanNativeApprovedApprovalCheckpointsDecisionExecutingUnderFence(t *testing.T) {
	t.Parallel()
	h := newDecisionHarness(t, planApprovalToolName)
	approval, input := planApproval(toolapproval.StatusApproved)
	ctx := fencedContext(context.Background(), 9)
	cont := approvalContinuation(ctx, h, approval, input, true)
	if _, err := cont.Setup(ctx, cont.Req); err != nil {
		t.Fatalf("approval setup error = %v", err)
	}
	got := h.queries.recorded()
	if len(got) != 1 {
		t.Fatalf("MarkSessionRunDecisionExecuting calls = %d, want 1", len(got))
	}
	if got[0].DecisionID != approval.ID || got[0].FencingToken != 9 {
		t.Fatalf("checkpoint = %#v, want decision %q under token 9", got[0], approval.ID)
	}
	if runreq.PGUUIDString(got[0].RunID) != agenttest.LifecycleRunID ||
		runreq.PGUUIDString(got[0].BotID) != agenttest.LifecycleBotID ||
		runreq.PGUUIDString(got[0].SessionID) != agenttest.LifecycleSessionID {
		t.Fatalf("checkpoint scope = %#v, want the lifecycle run/bot/session", got[0])
	}
	if len(h.tool.calls()) != 1 {
		t.Fatalf("tool calls = %d, want 1 after a successful checkpoint", len(h.tool.calls()))
	}
}

func TestPlanNativeApprovedApprovalLosesRunWhenCheckpointDoesNotApply(t *testing.T) {
	t.Parallel()
	h := newDecisionHarness(t, planApprovalToolName)
	h.queries.count = 0
	approval, input := planApproval(toolapproval.StatusApproved)
	ctx := fencedContext(context.Background(), 9)
	cont := approvalContinuation(ctx, h, approval, input, true)
	_, err := cont.Setup(ctx, cont.Req)
	if !errors.Is(err, sessionruntime.ErrRunOwnershipLost) {
		t.Fatalf("approval setup error = %v, want ErrRunOwnershipLost", err)
	}
	if len(h.tool.calls()) != 0 {
		t.Fatal("the tool ran although its decision could not be checkpointed")
	}
	if len(toolRows(h)) != 0 {
		t.Fatal("a tool row was stored for a run that lost ownership")
	}
}

func TestPlanNativeApprovedApprovalWithoutFenceSkipsCheckpoint(t *testing.T) {
	t.Parallel()
	h := newDecisionHarness(t, planApprovalToolName)
	approval, input := planApproval(toolapproval.StatusApproved)
	cont := approvalContinuation(context.Background(), h, approval, input, true)
	if _, err := cont.Setup(context.Background(), cont.Req); err != nil {
		t.Fatalf("approval setup error = %v", err)
	}
	if calls := h.queries.recorded(); len(calls) != 0 {
		t.Fatalf("MarkSessionRunDecisionExecuting calls = %d, want 0 without a fence", len(calls))
	}
	if len(h.tool.calls()) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(h.tool.calls()))
	}
}

func TestPlanNativeRejectedApprovalStoresRejectionAsErrorResult(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, reason, want string
	}{
		{"with reason", "too risky", "tool execution rejected by user: too risky"},
		{"without reason", "", "tool execution rejected by user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newDecisionHarness(t, planApprovalToolName)
			approval, input := planApproval(toolapproval.StatusRejected)
			input.Reason = tc.reason
			cont := approvalContinuation(context.Background(), h, approval, input, true)
			if _, err := cont.Setup(context.Background(), cont.Req); err != nil {
				t.Fatalf("approval setup error = %v", err)
			}
			if len(h.tool.calls()) != 0 {
				t.Fatal("a rejected tool was executed")
			}
			rows := toolRows(h)
			if len(rows) != 1 {
				t.Fatalf("persisted tool rows = %d, want 1", len(rows))
			}
			if !strings.Contains(string(rows[0].Content), tc.want) {
				t.Fatalf("row content = %s, want %q", rows[0].Content, tc.want)
			}
			if strings.HasSuffix(tc.want, "user") && strings.Contains(string(rows[0].Content), "user:") {
				t.Fatalf("row content = %s, want no reason suffix", rows[0].Content)
			}
			if !toolRowIsError(t, rows[0]) {
				t.Fatalf("rejected row is not an error result: %s", rows[0].Content)
			}
		})
	}
}

// The history a resumed run reads is the session the decision parked in: the
// compaction summaries it carries are looked up under that session.
func TestPlanNativeContinuationReadsTheSessionsCompactionSummaries(t *testing.T) {
	t.Parallel()
	h := newDecisionHarness(t, planApprovalToolName)
	approval, input := planApproval(toolapproval.StatusRejected)
	cont := approvalContinuation(context.Background(), h, approval, input, true)
	if _, err := cont.Setup(context.Background(), cont.Req); err != nil {
		t.Fatalf("approval setup error = %v", err)
	}
	want := db.ParseUUIDOrEmpty(agenttest.LifecycleSessionID)
	got := h.queries.lineageSessions()
	if len(got) == 0 {
		t.Fatal("the resumed run did not read the session's compaction lineage")
	}
	for _, session := range got {
		if session != want {
			t.Fatalf("compaction lineage read for session %v, want %v", session, want)
		}
	}
}

// toolRowIsError reads the error flag of the stored tool result.
func toolRowIsError(t *testing.T, row messagepkg.PersistInput) bool {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(row.Content, &raw); err != nil {
		t.Fatalf("decode row content %s: %v", row.Content, err)
	}
	return findErrorFlag(raw)
}

func findErrorFlag(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if (k == "is_error" || k == "isError") && val == true {
				return true
			}
			if findErrorFlag(val) {
				return true
			}
		}
	case []any:
		for _, val := range x {
			if findErrorFlag(val) {
				return true
			}
		}
	}
	return false
}

func TestPlanNativeRejectedApprovalLimitsTheRejectionText(t *testing.T) {
	t.Parallel()
	h := newDecisionHarness(t, planApprovalToolName)
	approval, input := planApproval(toolapproval.StatusRejected)
	input.Reason = "HEAD\n" + strings.Repeat("rejection detail ", 300) + "\nTAIL"
	cont := approvalContinuation(context.Background(), h, approval, input, true)
	if _, err := cont.Setup(context.Background(), cont.Req); err != nil {
		t.Fatalf("approval setup error = %v", err)
	}
	rows := toolRows(h)
	if len(rows) != 1 {
		t.Fatalf("persisted tool rows = %d, want 1", len(rows))
	}
	content := string(rows[0].Content)
	if len(content) >= len(input.Reason) {
		t.Fatalf("rejection text was not limited: %d bytes stored, reason is %d", len(content), len(input.Reason))
	}
	if !strings.Contains(content, "tool execution rejected by user: HEAD") || !strings.Contains(content, "[memoh pruned]") {
		t.Fatalf("limited rejection = %s, want the prefix, the head and the prune marker", content)
	}
}

func TestPlanNativeApprovalWithUnexpectedStatusFailsBeforeStoring(t *testing.T) {
	t.Parallel()
	h := newDecisionHarness(t, planApprovalToolName)
	approval, input := planApproval("pending")
	cont := approvalContinuation(context.Background(), h, approval, input, true)
	_, err := cont.Setup(context.Background(), cont.Req)
	const want = `committed tool approval has unexpected status "pending"`
	if err == nil || err.Error() != want {
		t.Fatalf("approval setup error = %v, want %q", err, want)
	}
	if len(h.history.Persisted) != 0 {
		t.Fatalf("persisted rows = %d, want none", len(h.history.Persisted))
	}
}

func TestPlanNativeUserInputContinuationStoresAnswerAsSuccessfulLimitedResult(t *testing.T) {
	t.Parallel()
	large := "HEAD\n" + strings.Repeat("answer detail ", 300) + "\nTAIL"
	h := newDecisionHarness(t, "ask_user")
	answered := planAnswer("telegram", "im-chat-1")
	answered.Result = map[string]any{"answers": []any{map[string]any{"text": large}}}
	cont := userInputContinuation(context.Background(), h, answered, true)
	if _, err := cont.Setup(context.Background(), cont.Req); err != nil {
		t.Fatalf("user input setup error = %v", err)
	}
	rows := toolRows(h)
	if len(rows) != 1 {
		t.Fatalf("persisted tool rows = %d, want 1", len(rows))
	}
	content := string(rows[0].Content)
	if len(content) >= len(large) || !strings.Contains(content, "[memoh pruned]") || !strings.Contains(content, "HEAD") {
		t.Fatalf("answer row = %s, want the answer pruned with its head kept", content)
	}
	if toolRowIsError(t, rows[0]) {
		t.Fatalf("answer row is marked as an error: %s", content)
	}
}

func TestPlanNativeUserInputContinuationStoresPlainAnswer(t *testing.T) {
	t.Parallel()
	h := newDecisionHarness(t, "ask_user")
	cont := userInputContinuation(context.Background(), h, planAnswer("telegram", "im-chat-1"), true)
	if _, err := cont.Setup(context.Background(), cont.Req); err != nil {
		t.Fatalf("user input setup error = %v", err)
	}
	rows := toolRows(h)
	if len(rows) != 1 || !strings.Contains(string(rows[0].Content), "blue") {
		t.Fatalf("tool rows = %v, want one carrying the answer", rows)
	}
	if toolRowIsError(t, rows[0]) {
		t.Fatalf("answer row is marked as an error: %s", rows[0].Content)
	}
}

func TestPlanNativeContinuationRequestFieldsMatchTheCommittedRequest(t *testing.T) {
	t.Parallel()
	runBoth := func(t *testing.T, build func(h decisionHarness) decisionContinuation, toolName string) (decisionContinuation, drive.RunPlan) {
		h := newDecisionHarness(t, toolName)
		cont := build(h)
		driver := cont.Req
		driver.RunID = "driver-run-id"
		plan, err := cont.Setup(context.Background(), driver)
		if err != nil {
			t.Fatalf("setup error = %v", err)
		}
		return cont, plan
	}
	cases := []struct {
		name    string
		skipMem bool
		tool    string
		build   func(h decisionHarness) decisionContinuation
	}{
		{"user input", true, "ask_user", func(h decisionHarness) decisionContinuation {
			return userInputContinuation(context.Background(), h, planAnswer("telegram", "im-chat-1"), true)
		}},
		{"tool approval", false, planApprovalToolName, func(h decisionHarness) decisionContinuation {
			approval, input := planApproval(toolapproval.StatusApproved)
			return approvalContinuation(context.Background(), h, approval, input, true)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cont, plan := runBoth(t, tc.build, tc.tool)
			if plan.Req.SkipMemoryExtraction != tc.skipMem {
				t.Fatalf("plan.Req.SkipMemoryExtraction = %v, want %v", plan.Req.SkipMemoryExtraction, tc.skipMem)
			}
			if cont.Req.SkipMemoryExtraction != tc.skipMem {
				t.Fatalf("cont.Req.SkipMemoryExtraction = %v, want %v", cont.Req.SkipMemoryExtraction, tc.skipMem)
			}
			if !plan.Req.UserMessagePersisted || plan.Req.PersistedUserMessageID != driveRequestMessageID {
				t.Fatalf("plan.Req persisted user message = %v/%q, want true/%q",
					plan.Req.UserMessagePersisted, plan.Req.PersistedUserMessageID, driveRequestMessageID)
			}
			if plan.Req.PersistedUserMessageID != cont.Req.PersistedUserMessageID ||
				plan.Req.ThreadID != cont.Req.ThreadID || plan.Req.ThreadID != agenttest.LifecycleSessionID ||
				plan.Req.ChatID != agenttest.LifecycleBotID || plan.Req.BotID != agenttest.LifecycleBotID ||
				plan.Req.WorkspaceTargetID != cont.Req.WorkspaceTargetID ||
				plan.Req.CurrentChannel != "telegram" || plan.Req.ReplyTarget != "im-chat-1" ||
				plan.Req.ConversationType != "private" {
				t.Fatalf("plan.Req = %#v does not match the committed request %#v", plan.Req, cont.Req)
			}
			// The resumed run keeps the run id of the request the driver handed
			// setup, not the one the continuation was built with.
			if plan.Req.RunID != "driver-run-id" {
				t.Fatalf("plan.Req.RunID = %q, want the driver's %q", plan.Req.RunID, "driver-run-id")
			}
			if cont.Req.RunID != agenttest.LifecycleRunID {
				t.Fatalf("cont.Req.RunID = %q, want %q", cont.Req.RunID, agenttest.LifecycleRunID)
			}
		})
	}
}

func TestPlanNativeContinuationFailsBeforeAnyActionWhenTheRequestMessageIsUnreadable(t *testing.T) {
	t.Parallel()
	for _, kind := range decisionKinds {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			h := newDecisionHarness(t, planApprovalToolName)
			unreadable := errors.New("transcript cannot name the request message")
			h.history.err = unreadable

			var cont decisionContinuation
			ctx := fencedContext(context.Background(), 9)
			if kind == kindApproval {
				approval, input := planApproval(toolapproval.StatusApproved)
				cont = approvalContinuation(ctx, h, approval, input, true)
			} else {
				cont = userInputContinuation(ctx, h, planAnswer("web", ""), true)
			}
			_, err := cont.Setup(ctx, cont.Req)
			if !errors.Is(err, unreadable) {
				t.Fatalf("setup error = %v, want the transcript's error", err)
			}
			if len(h.tool.calls()) != 0 || len(h.history.Persisted) != 0 {
				t.Fatalf("setup acted before failing: tool calls %d, persisted %d", len(h.tool.calls()), len(h.history.Persisted))
			}
			if len(h.queries.recorded()) != 0 {
				t.Fatal("setup checkpointed the decision before failing")
			}
		})
	}
}

func TestPlanNativeContinuationRejectsAnEmptyDecisionID(t *testing.T) {
	t.Parallel()
	t.Run("tool approval", func(t *testing.T) {
		t.Parallel()
		h := newDecisionHarness(t, planApprovalToolName)
		approval, input := planApproval(toolapproval.StatusApproved)
		approval.ID = " "
		cont := approvalContinuation(context.Background(), h, approval, input, true)
		_, err := cont.Setup(context.Background(), cont.Req)
		const want = "committed tool approval response is missing its request"
		if err == nil || err.Error() != want {
			t.Fatalf("setup error = %v, want %q", err, want)
		}
		if len(h.history.Persisted) != 0 || len(h.tool.calls()) != 0 {
			t.Fatal("setup acted on a response with no request")
		}
	})
	t.Run("user input", func(t *testing.T) {
		t.Parallel()
		h := newDecisionHarness(t, "ask_user")
		answered := planAnswer("telegram", "im-chat-1")
		answered.ID = ""
		cont := userInputContinuation(context.Background(), h, answered, true)
		_, err := cont.Setup(context.Background(), cont.Req)
		const want = "committed user input response is missing its request"
		if err == nil || err.Error() != want {
			t.Fatalf("setup error = %v, want %q", err, want)
		}
		if len(h.history.Persisted) != 0 {
			t.Fatal("setup stored a result for a response with no request")
		}
	})
}

func TestPlanNativeContinuationNormalizesAWebReplyTargetToTheBot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, platform, target, want string
	}{
		{"web without target", "web", "", agenttest.LifecycleBotID},
		{"web with target", "web", "kept-target", "kept-target"},
		{"other platform without target", "telegram", "", ""},
	}
	for _, kind := range decisionKinds {
		for _, tc := range cases {
			t.Run(string(kind)+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				var h decisionHarness
				var cont decisionContinuation
				if kind == kindApproval {
					h = newDecisionHarness(t, planApprovalToolName)
					approval, input := planApproval(toolapproval.StatusApproved)
					approval.SourcePlatform, approval.ReplyTarget = tc.platform, tc.target
					cont = approvalContinuation(context.Background(), h, approval, input, true)
				} else {
					h = newDecisionHarness(t, "ask_user")
					cont = userInputContinuation(context.Background(), h, planAnswer(tc.platform, tc.target), true)
				}
				if cont.Req.ReplyTarget != tc.want {
					t.Fatalf("cont.Req.ReplyTarget = %q, want %q", cont.Req.ReplyTarget, tc.want)
				}
				plan, err := cont.Setup(context.Background(), cont.Req)
				if err != nil {
					t.Fatalf("setup error = %v", err)
				}
				if plan.Req.ReplyTarget != tc.want {
					t.Fatalf("plan.Req.ReplyTarget = %q, want %q", plan.Req.ReplyTarget, tc.want)
				}
				if plan.Req.CurrentChannel != tc.platform {
					t.Fatalf("plan.Req.CurrentChannel = %q, want %q", plan.Req.CurrentChannel, tc.platform)
				}
			})
		}
	}
}

func TestPlanNativeContinuationKeepsDriverRunIDAndResolvedWorkspaceTarget(t *testing.T) {
	t.Parallel()
	for _, kind := range decisionKinds {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			var h decisionHarness
			var cont decisionContinuation
			if kind == kindApproval {
				h = newDecisionHarness(t, planApprovalToolName)
				approval, input := planApproval(toolapproval.StatusApproved)
				cont = approvalContinuation(context.Background(), h, approval, input, true)
			} else {
				h = newDecisionHarness(t, "ask_user")
				cont = userInputContinuation(context.Background(), h, planAnswer("telegram", "im-chat-1"), true)
			}
			driver := cont.Req
			driver.RunID = "driver-run-id"
			plan, err := cont.Setup(context.Background(), driver)
			if err != nil {
				t.Fatalf("setup error = %v", err)
			}
			execute(context.Background(), plan)
			cfg := h.capture.first(t)
			want := runconfig.WorkspaceTargetFromRunConfig(cfg)
			if want == nil || want.TargetID != planWorkspaceTargetID || want.Kind != "container" {
				t.Fatalf("resolved run config workspace target = %#v, want the plan target", want)
			}
			if !reflect.DeepEqual(plan.Req.WorkspaceTarget, want) {
				t.Fatalf("plan.Req.WorkspaceTarget = %#v, want the resolved run config's %#v", plan.Req.WorkspaceTarget, want)
			}
			if cfg.RunID != "driver-run-id" || plan.Req.RunID != "driver-run-id" {
				t.Fatalf("cfg.RunID = %q, plan.Req.RunID = %q, want the driver's %q", cfg.RunID, plan.Req.RunID, "driver-run-id")
			}
		})
	}
}
