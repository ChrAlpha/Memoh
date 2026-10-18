package plan

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/compact"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/transcript"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/runtimefence"
	sessiontest "github.com/felinics/memoh/internal/testutil/sessionruntime"
)

// stepMessages is a message store that writes steps, so a run over it can have
// a step committer.
type stepMessages struct {
	agenttest.RecordingMessages
}

func (*stepMessages) PersistAgentStep(context.Context, messagepkg.AgentStep) ([]messagepkg.Message, error) {
	return nil, nil
}

// nopAgent is an agent no test run reaches.
type nopAgent struct{ fixedStream }

func (nopAgent) ExecuteToolWithUIMetadata(context.Context, native.RunConfig, sdk.ToolCall) (sdk.ToolResultPart, map[string]any, error) {
	return sdk.ToolResultPart{}, nil, errors.New("unexpected tool execution")
}

// tailPlanner is a planner over stand-ins, enough to build the tail of a native
// run on messages and the runtime.
func tailPlanner(t *testing.T, messages messagepkg.Service, manager *sessionruntime.Manager) *Planner {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	reader := transcript.New(transcript.Deps{Logger: logger})
	return New(Deps{
		Logger:    logger,
		Agent:     nopAgent{},
		Runtime:   manager,
		Persist:   testWriterOn(messages, manager),
		Compact:   compact.New(compact.Deps{Logger: logger, Transcript: reader}),
		Runconfig: newTestBuilder(),
		Lifecycle: lifecycle.New(lifecycle.Deps{
			Store: &agenttest.RecordingLifecycleStore{}, Runs: agenttest.NoRuns{}, Decisions: agenttest.NoDecisions{}, Logger: logger,
		}),
	})
}

func tailManager(t *testing.T) *sessionruntime.Manager {
	t.Helper()
	manager := sessiontest.New(sessionruntime.NewMemoryBackend(), sessionruntime.Options{OwnerID: "owner-1", OwnerLeaseTTL: time.Minute})
	t.Cleanup(func() { _ = manager.Close() })
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("start session manager: %v", err)
	}
	return manager
}

// fencedContext carries the fence a step committer needs.
func fencedContext(token int64) context.Context {
	return runtimefence.WithContext(context.Background(), runtimefence.Fence{
		BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID, Token: token,
	})
}

func tailRequest() runreq.ChatRequest {
	req := resumedRequest()
	req.RunID = agenttest.LifecycleRunID
	return req
}

func tailExecutor(t *testing.T, plan drive.RunPlan) *nativeExecutor {
	t.Helper()
	exec, ok := plan.Exec.(*nativeExecutor)
	if !ok {
		t.Fatalf("plan executor = %T, want *nativeExecutor", plan.Exec)
	}
	return exec
}

// A native run's flags are the entry's: a chat follows its tool stream and can
// ask a question, a schedule does neither, and a resumed run does both exactly
// when someone follows it.
func TestNativePlanAppliesTheEntrysInteractionFlags(t *testing.T) {
	t.Parallel()

	planner := tailPlanner(t, &agenttest.RecordingMessages{}, tailManager(t))
	for _, tc := range []struct {
		name       string
		live, ask  bool
		wantLive   bool
		wantAsking bool
	}{
		{name: "live", live: true, ask: true, wantLive: true, wantAsking: true},
		{name: "unattended", wantLive: false, wantAsking: false},
		{name: "streamed without questions", live: true, wantLive: true},
		{name: "questions without a stream", ask: true, wantAsking: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The entry's own cfg may carry the opposite of what it asks for.
			cfg := native.RunConfig{LiveToolStream: !tc.live, CanRequestUserInput: !tc.ask}
			plan, err := planner.nativePlan(context.Background(), tailRequest(), runreq.ResolvedContext{}, cfg, nativeOptions{
				LiveToolStream: tc.live, CanRequestUserInput: tc.ask, Steps: stepsOff,
			})
			if err != nil {
				t.Fatalf("nativePlan() error = %v", err)
			}
			got := tailExecutor(t, plan).cfg
			if got.LiveToolStream != tc.wantLive || got.CanRequestUserInput != tc.wantAsking {
				t.Fatalf("flags = live:%t input:%t, want live:%t input:%t", got.LiveToolStream, got.CanRequestUserInput, tc.wantLive, tc.wantAsking)
			}
		})
	}
}

// The tail compiles the prompt itself, so what an entry left in the context
// from an earlier assembly does not reach the run.
func TestNativePlanCompilesThePromptOverStaleSourceFragments(t *testing.T) {
	t.Parallel()

	planner := tailPlanner(t, &agenttest.RecordingMessages{}, tailManager(t))
	cfg := native.RunConfig{ContextSourceFrags: []contextfrag.ContextFrag{{ID: "stale-source-fragment", Kind: contextfrag.KindConversationEvent}}}
	plan, err := planner.nativePlan(context.Background(), tailRequest(), runreq.ResolvedContext{}, cfg, nativeOptions{Steps: stepsOff})
	if err != nil {
		t.Fatalf("nativePlan() error = %v", err)
	}
	for _, frag := range tailExecutor(t, plan).cfg.ContextSourceFrags {
		if frag.ID == "stale-source-fragment" {
			t.Fatalf("the run kept a stale source fragment: %#v", frag)
		}
	}
	if tailExecutor(t, plan).cfg.System == "" {
		t.Fatal("the run has no system prompt: the tail did not compile one")
	}
}

// How a run commits its steps is the entry's: off writes the history from the
// terminal snapshot alone and does not even time reasoning, a fresh run gets a
// committer when it can have one, and a resumed run without a live queue to
// join gets none but still times its reasoning.
func TestNativePlanStepModes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		steps         stepMode
		ctx           context.Context
		wantCommitter bool
		wantTiming    bool
	}{
		{name: "off", steps: stepsOff, ctx: fencedContext(1)},
		{name: "fresh with a fence", steps: stepsFresh, ctx: fencedContext(1), wantCommitter: true, wantTiming: true},
		{name: "fresh without a fence", steps: stepsFresh, ctx: context.Background(), wantTiming: true},
		{name: "resume without an owned run", steps: stepsResume, ctx: fencedContext(1), wantTiming: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			planner := tailPlanner(t, &stepMessages{}, tailManager(t))
			req := tailRequest()
			plan, err := planner.nativePlan(tc.ctx, req, runreq.ResolvedContext{}, native.RunConfig{}, nativeOptions{Steps: tc.steps})
			if err != nil {
				t.Fatalf("nativePlan() error = %v", err)
			}
			exec := tailExecutor(t, plan)
			if (exec.committer != nil) != tc.wantCommitter {
				t.Fatalf("committer = %v, want present=%t", exec.committer, tc.wantCommitter)
			}
			if (exec.cfg.OnProviderStreamEventObserved != nil) != tc.wantTiming || (exec.cfg.OnStepCommitted != nil) != tc.wantTiming || (exec.cfg.OnStepInterrupted != nil) != tc.wantTiming {
				t.Fatalf("reasoning timing hooks = observed:%t committed:%t interrupted:%t, want all %t",
					exec.cfg.OnProviderStreamEventObserved != nil, exec.cfg.OnStepCommitted != nil, exec.cfg.OnStepInterrupted != nil, tc.wantTiming)
			}
			if plan.Req.StepIndexOffset != 0 || plan.Req.QueueSteerEnabled {
				t.Fatalf("request = offset:%d steer:%t, want a request left as it came", plan.Req.StepIndexOffset, plan.Req.QueueSteerEnabled)
			}
		})
	}
}

// A resumed run joins the live queue of the run it continues: its steps are
// numbered after the ones already committed, and its committer owns the steer
// hooks.
func TestNativePlanResumeContinuesTheParkedRunsSteps(t *testing.T) {
	t.Parallel()

	manager := tailManager(t)
	handle, err := sessiontest.Start(context.Background(), manager, agenttest.LifecycleBotID, agenttest.LifecycleSessionID, agenttest.LifecycleRunID,
		make(chan struct{}, 1), func() {}, nil)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if _, err := manager.HandleAgentEvent(context.Background(), handle, native.StreamEvent{Type: native.EventStepEnd, StepNumber: 2}); err != nil {
		t.Fatalf("mark steps consumed: %v", err)
	}
	planner := tailPlanner(t, &stepMessages{}, manager)
	req := tailRequest()
	req.RunHandle = handle

	plan, err := planner.nativePlan(fencedContext(handle.FencingToken), req, runreq.ResolvedContext{}, native.RunConfig{}, nativeOptions{Steps: stepsResume})
	if err != nil {
		t.Fatalf("nativePlan() error = %v", err)
	}
	exec := tailExecutor(t, plan)
	if exec.committer == nil {
		t.Fatal("the resumed run has no step committer")
	}
	if plan.Req.StepIndexOffset != 3 || exec.cfg.StepIndexOffset != 3 || exec.req.StepIndexOffset != 3 {
		t.Fatalf("step offset = request:%d config:%d executor:%d, want 3 (steps 0 through 2 are committed)", plan.Req.StepIndexOffset, exec.cfg.StepIndexOffset, exec.req.StepIndexOffset)
	}
	if !plan.Req.QueueSteerEnabled {
		t.Fatal("the resumed request does not take steers")
	}
	if exec.cfg.PendingSteer == nil || exec.cfg.OnSteer == nil {
		t.Fatalf("steer hooks = pending:%t steered:%t, want the committer's", exec.cfg.PendingSteer != nil, exec.cfg.OnSteer != nil)
	}
}

// A resumed run whose handle names a run this process does not own cannot
// number its steps, and a run that owns its queue but cannot commit steps cannot
// join it. Either is the resumed run's failure.
func TestNativePlanResumeFailsWithoutALiveQueue(t *testing.T) {
	t.Parallel()

	t.Run("run not owned", func(t *testing.T) {
		t.Parallel()
		req := tailRequest()
		req.RunHandle = sessionruntime.RunHandle{
			BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID, RunID: agenttest.LifecycleRunID,
			OwnerID: "owner-1", FencingToken: 1,
		}
		_, err := tailPlanner(t, &stepMessages{}, tailManager(t)).nativePlan(fencedContext(1), req, runreq.ResolvedContext{}, native.RunConfig{}, nativeOptions{Steps: stepsResume})
		if !errors.Is(err, sessionruntime.ErrRunOwnershipLost) {
			t.Fatalf("nativePlan() error = %v, want %v", err, sessionruntime.ErrRunOwnershipLost)
		}
	})

	t.Run("no committer to join with", func(t *testing.T) {
		t.Parallel()
		manager := tailManager(t)
		handle, err := sessiontest.Start(context.Background(), manager, agenttest.LifecycleBotID, agenttest.LifecycleSessionID, agenttest.LifecycleRunID,
			make(chan struct{}, 1), func() {}, nil)
		if err != nil {
			t.Fatalf("start run: %v", err)
		}
		req := tailRequest()
		req.RunHandle = handle
		// A message store that cannot write steps leaves no committer.
		_, err = tailPlanner(t, &agenttest.RecordingMessages{}, manager).nativePlan(fencedContext(handle.FencingToken), req, runreq.ResolvedContext{}, native.RunConfig{}, nativeOptions{Steps: stepsResume})
		if err == nil || err.Error() != "live queue step committer is unavailable for decision continuation" {
			t.Fatalf("nativePlan() error = %v, want the missing committer", err)
		}
	})
}

// A retry or an edit run carries the turn it replaces into its executor.
func TestNativePlanCarriesTheReplacement(t *testing.T) {
	t.Parallel()

	replacement := &drive.TurnReplacement{}
	planner := tailPlanner(t, &agenttest.RecordingMessages{}, tailManager(t))
	plan, err := planner.nativePlan(context.Background(), tailRequest(), runreq.ResolvedContext{}, native.RunConfig{}, nativeOptions{Steps: stepsOff, Replacement: replacement})
	if err != nil {
		t.Fatalf("nativePlan() error = %v", err)
	}
	if tailExecutor(t, plan).replace != replacement {
		t.Fatal("the executor does not carry the replacement")
	}
	plan, err = planner.nativePlan(context.Background(), tailRequest(), runreq.ResolvedContext{}, native.RunConfig{}, nativeOptions{Steps: stepsOff})
	if err != nil {
		t.Fatalf("nativePlan() error = %v", err)
	}
	if tailExecutor(t, plan).replace != nil {
		t.Fatal("a run that replaces nothing carries a replacement")
	}
}
