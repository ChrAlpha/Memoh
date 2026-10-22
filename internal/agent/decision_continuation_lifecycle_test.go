package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/answer"
	"github.com/felinics/memoh/internal/agent/decide"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

type unavailableContinuationMessages struct {
	*agenttest.RecordingMessages
	err   error
	calls int
}

func (s *unavailableContinuationMessages) Persist(
	context.Context,
	messagepkg.PersistInput,
) (messagepkg.Message, error) {
	s.calls++
	return messagepkg.Message{}, s.err
}

// PersistRound refuses the rows of an assistant: the stored answer of the
// decision is a tool row, which is written.
func (s *unavailableContinuationMessages) PersistRound(
	ctx context.Context,
	inputs []messagepkg.PersistInput,
	options messagepkg.RoundPersistenceOptions,
) ([]messagepkg.Message, bool, error) {
	for _, input := range inputs {
		if input.Role == "assistant" {
			s.calls++
			return nil, true, s.err
		}
	}
	return s.RecordingMessages.PersistRound(ctx, inputs, options)
}

// PersistAgentStep refuses the rows of an assistant the same way.
func (s *unavailableContinuationMessages) PersistAgentStep(ctx context.Context, step messagepkg.AgentStep) ([]messagepkg.Message, error) {
	for _, input := range step.Messages {
		if input.Role == "assistant" {
			s.calls++
			return nil, s.err
		}
	}
	return s.RecordingMessages.PersistAgentStep(ctx, step)
}

type continuationRunConfigCapture struct {
	mu     sync.Mutex
	calls  int
	budget int
	runID  string
}

func (c *continuationRunConfigCapture) apply(
	_ context.Context,
	cfg native.RunConfig,
) native.RunConfig {
	c.mu.Lock()
	c.calls++
	c.budget = cfg.ContextBudgetMaxTokens
	c.runID = cfg.RunID
	c.mu.Unlock()
	return cfg.RefreshContextFrag()
}

func (c *continuationRunConfigCapture) snapshot() (calls, budget int, runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.budget, c.runID
}

// continuationSessions are the two decision kinds' resolutions of the run an
// answer resumes, each called the way its continuation's setup calls it.
var continuationSessions = []struct {
	name    string
	resolve func(context.Context, *Service, runreq.ChatRequest) (drive.RunPlan, error)
}{
	{
		name: "user input",
		resolve: func(ctx context.Context, service *Service, req runreq.ChatRequest) (drive.RunPlan, error) {
			return service.plan.Continue(ctx, decide.DecidedCall{
				Kind: decide.CallUserInput, Verdict: decide.VerdictAnswered,
				RunID: req.RunID, DecisionID: "user-input", BotID: agenttest.LifecycleBotID, ScopeBotID: agenttest.LifecycleBotID,
				SessionID: agenttest.LifecycleSessionID, SourcePlatform: "web",
				ToolCallID: "ask-user-call", ToolName: "ask_user",
				Result: sdk.ToolResultPart{ToolCallID: "ask-user-call", ToolName: "ask_user", Result: sdk.TextOutput("answer")},
			}, req, false)
		},
	},
	{
		name: "tool approval",
		resolve: func(ctx context.Context, service *Service, req runreq.ChatRequest) (drive.RunPlan, error) {
			return service.plan.Continue(ctx, decide.DecidedCall{
				Kind: decide.CallToolApproval, Verdict: decide.VerdictRejected,
				RunID: req.RunID, DecisionID: "tool-approval", BotID: agenttest.LifecycleBotID, ScopeBotID: agenttest.LifecycleBotID,
				SessionID: agenttest.LifecycleSessionID, SourcePlatform: "web",
				ToolCallID: "approved-tool-call", ToolName: "container_exec",
				Result: sdk.ToolResultPart{ToolCallID: "approved-tool-call", ToolName: "container_exec", Result: sdk.TextOutput("rejected"), IsError: true},
			}, req, false)
		},
	},
}

func TestRuntimeDecisionContinuationsPropagateResolvedBudgetAndAdmittedRunIDToAgentStream(t *testing.T) {
	for _, tt := range continuationSessions {
		t.Run(tt.name, func(t *testing.T) {
			capture := &continuationRunConfigCapture{}
			manager := newPlanManager(t)
			fixture := newDirectLifecycleFixture(t, directLifecycleModelSuccess, withFixtureDeps(func(d *Deps) {
				d.Agent = native.New(native.Deps{
					Logger:             d.Logger,
					ContextViewApplier: capture.apply,
				})
			}), withFixtureOptions(withSessionManager(manager)))
			req := continuationSessionRequest()
			req.RunHandle = startPlanRun(t, manager, -1)
			ctx := fencedContext(context.Background(), req.RunHandle.FencingToken)

			plan, err := tt.resolve(ctx, fixture.service, req)
			if err != nil {
				t.Fatalf("continuation setup error = %v", err)
			}
			plan.Exec.Execute(ctx, func(native.StreamEvent) {})
			calls, budget, runID := capture.snapshot()
			if calls == 0 {
				t.Fatal("ContextView applier was not reached through agent.Stream")
			}
			if budget != 128000 {
				t.Fatalf("ContextBudgetMaxTokens seen by agent.Stream = %d, want 128000", budget)
			}
			if runID != agenttest.LifecycleRunID {
				t.Fatalf("RunID seen by agent.Stream = %q, want admitted run ID %q", runID, agenttest.LifecycleRunID)
			}
		})
	}
}

// A continuation whose assistant rows cannot be written still leaves the
// lifecycle row of the context it assembled: the run's own lifecycle carries
// it, not a fallback recovered from the rows that were never written.
func TestRuntimeOwnedDecisionContinuationsRetainLifecycleWithoutAssistantMetadata(t *testing.T) {
	for _, tt := range continuationSessions {
		t.Run(tt.name, func(t *testing.T) {
			messages := &unavailableContinuationMessages{
				RecordingMessages: &agenttest.RecordingMessages{},
				err:               errors.New("assistant message store unavailable"),
			}
			manager := newPlanManager(t)
			fixture := newDirectLifecycleFixture(t, directLifecycleModelSuccess, withFixtureMessages(messages),
				withFixtureOptions(withSessionManager(manager)))
			req := continuationSessionRequest()
			req.RunHandle = startPlanRun(t, manager, -1)
			ctx := fencedContext(context.Background(), req.RunHandle.FencingToken)

			err := fixture.service.answer.DriveUnrouted(ctx, answer.Continuation{
				Req: req,
				Setup: func(ctx context.Context, _ runreq.ChatRequest) (drive.RunPlan, error) {
					return tt.resolve(ctx, fixture.service, req)
				},
			}, nil)
			// A step the committer cannot write is a history write failure.
			if apperror.CodeOf(err) != apperror.CodeSessionHistoryInconsistent {
				t.Fatalf("continuation error = %v, want %s", err, apperror.CodeSessionHistoryInconsistent)
			}
			if messages.calls == 0 {
				t.Fatal("test did not exercise unavailable assistant persistence")
			}
			// The session runtime tells the recorder how the run it admitted
			// ended; this run was driven without one, so the test does.
			fixture.service.lifecycle.ReconcileTerminal(ctx, sessionruntime.TerminalRun{
				RunID: req.RunHandle.RunID, BotID: req.RunHandle.BotID, SessionID: req.RunHandle.SessionID,
				FencingToken: req.RunHandle.FencingToken, State: "failed", ErrorCode: string(apperror.CodeSessionHistoryInconsistent), Applied: true,
			})
			assertDirectLifecycle(
				t,
				fixture.lifecycles,
				agenttest.LifecycleRunID,
				lifecycle.StatusFailedProvider,
				"",
			)
		})
	}
}
