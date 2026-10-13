package application

import (
	"context"
	"errors"
	"sync"
	"testing"

	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

type unavailableContinuationMessageService struct {
	*recordingMessageService
	err   error
	calls int
}

func (s *unavailableContinuationMessageService) Persist(
	context.Context,
	messagepkg.PersistInput,
) (messagepkg.Message, error) {
	s.calls++
	return messagepkg.Message{}, s.err
}

func (s *unavailableContinuationMessageService) PersistRound(
	context.Context,
	[]messagepkg.PersistInput,
	messagepkg.RoundPersistenceOptions,
) ([]messagepkg.Message, bool, error) {
	s.calls++
	return nil, true, s.err
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
	resolve func(context.Context, *Service, ChatRequest) (runPlan, error)
}{
	{
		name: "user input",
		resolve: func(ctx context.Context, service *Service, req ChatRequest) (runPlan, error) {
			return service.continueUserInputSession(ctx,
				userinput.Request{
					ID: "user-input", BotID: lifecycleTestBotID, SessionID: lifecycleTestSessionID,
					ToolCallID: "ask-user-call", ToolName: "ask_user", SourcePlatform: "web",
				},
				UserInputResponseInput{BotID: lifecycleTestBotID, ThreadID: lifecycleTestSessionID},
				req, false,
			)
		},
	},
	{
		name: "tool approval",
		resolve: func(ctx context.Context, service *Service, req ChatRequest) (runPlan, error) {
			return service.continueToolApprovalSession(ctx,
				toolapproval.Request{
					ID: "tool-approval", BotID: lifecycleTestBotID, SessionID: lifecycleTestSessionID,
					ToolCallID: "approved-tool-call", ToolName: "container_exec", SourcePlatform: "web",
				},
				ToolApprovalResponseInput{BotID: lifecycleTestBotID, ThreadID: lifecycleTestSessionID},
				req, false,
			)
		},
	},
}

func continuationSessionRequest() ChatRequest {
	return ChatRequest{
		RunID: lifecycleTestRunID, BotID: lifecycleTestBotID, ChatID: lifecycleTestBotID,
		ThreadID: lifecycleTestSessionID, CurrentChannel: "web", UserMessagePersisted: true,
	}
}

func TestRuntimeDecisionContinuationsPropagateResolvedBudgetAndAdmittedRunIDToAgentStream(t *testing.T) {
	for _, tt := range continuationSessions {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newDirectLifecycleFixture(t, directLifecycleModelSuccess)
			capture := &continuationRunConfigCapture{}
			fixture.service.agent = native.New(native.Deps{
				Logger:             fixture.service.logger,
				ContextViewApplier: capture.apply,
			})

			plan, err := tt.resolve(context.Background(), fixture.service, continuationSessionRequest())
			if err != nil {
				t.Fatalf("continuation setup error = %v", err)
			}
			plan.Exec.execute(context.Background(), func(native.StreamEvent) {})
			calls, budget, runID := capture.snapshot()
			if calls == 0 {
				t.Fatal("ContextView applier was not reached through agent.Stream")
			}
			if budget != 128000 {
				t.Fatalf("ContextBudgetMaxTokens seen by agent.Stream = %d, want 128000", budget)
			}
			if runID != lifecycleTestRunID {
				t.Fatalf("RunID seen by agent.Stream = %q, want admitted run ID %q", runID, lifecycleTestRunID)
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
			fixture := newDirectLifecycleFixture(t, directLifecycleModelSuccess)
			messages := &unavailableContinuationMessageService{
				recordingMessageService: &recordingMessageService{},
				err:                     errors.New("assistant message store unavailable"),
			}
			fixture.service.messageService = messages
			req := continuationSessionRequest()

			err := fixture.service.driveUnroutedContinuation(context.Background(), decisionContinuation{
				Req: req,
				Setup: func(ctx context.Context, _ ChatRequest) (runPlan, error) {
					return tt.resolve(ctx, fixture.service, req)
				},
			}, nil)
			if apperror.CodeOf(err) != apperror.CodeSessionHistoryInconsistent {
				t.Fatalf("continuation error = %v, want the history write failure", err)
			}
			if messages.calls == 0 {
				t.Fatal("test did not exercise unavailable assistant persistence")
			}
			assertDirectLifecycle(
				t,
				fixture.lifecycles,
				lifecycleTestRunID,
				contextLifecycleStatusFailedProvider,
				"",
			)
		})
	}
}
