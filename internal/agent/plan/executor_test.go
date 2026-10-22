package plan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agentapi"
	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/step"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/runtimefence"
)

// fixedStream is a native runtime that produces a fixed event sequence.
type fixedStream []native.StreamEvent

func (f fixedStream) Stream(context.Context, native.RunConfig) <-chan native.StreamEvent {
	ch := make(chan native.StreamEvent, len(f))
	for _, ev := range f {
		ch <- ev
	}
	close(ch)
	return ch
}

// testWriter is a history writer over messages and an in-memory session runtime.
func testWriter(t *testing.T, messages messagepkg.Service) *persist.Writer {
	t.Helper()
	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{})
	t.Cleanup(func() { _ = manager.Close() })
	return testWriterOn(messages, manager)
}

// testWriterOn is a history writer whose steers go through manager.
func testWriterOn(messages messagepkg.Service, manager *sessionruntime.Manager) *persist.Writer {
	logger := slog.New(slog.DiscardHandler)
	return persist.New(persist.Deps{
		Logger:   logger,
		Messages: messages,
		Sessions: agenttest.NewChatSessions(),
		Bot:      agenttest.BotEnv(logger),
		Runtime:  manager,
	})
}

// The native agent writes the step it was in when a stop arrived, as the stop
// unwinds the stream, so its ending has nothing left for the driver's stopped
// write. A hook here would write that step a second time.
func TestNativeExecutorLeavesTheStoppedWriteToTheRuntime(t *testing.T) {
	t.Parallel()

	exec := &nativeExecutor{
		agent: fixedStream{
			{Type: native.EventAgentStart},
			{Type: native.EventTextDelta, Delta: "partial"},
			{Type: native.EventAgentAbort, EndReason: event.EndCanceled},
		},
	}

	ending := exec.Execute(context.Background(), func(native.StreamEvent) {})
	if ending.CommitStopped != nil {
		t.Fatal("the native ending offers a stopped write, want the runtime's own step checkpoint to be the only one")
	}
}

// testCommitter is the step committer of a fenced run whose steps go to store.
func testCommitter(t *testing.T, store messagepkg.Service, req runreq.ChatRequest) *persist.StepCommitter {
	t.Helper()
	ctx := runtimefence.WithContext(context.Background(), runtimefence.Fence{
		BotID: req.BotID, SessionID: req.ThreadID, Token: 1,
	})
	committer := testWriter(t, store).NewStepCommitter(ctx, req, runreq.ResolvedContext{})
	if committer == nil {
		t.Fatal("missing step committer")
	}
	return committer
}

// testDriver is a driver over stand-ins whose history goes to messages.
func testDriver(t *testing.T, messages messagepkg.Service) *drive.Driver {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	return drive.New(drive.Deps{
		Logger:  logger,
		Runtime: &agenttest.ScriptedAdmitter{},
		Events:  &agenttest.EventRecorder{},
		Lifecycle: lifecycle.New(lifecycle.Deps{
			Store: &agenttest.RecordingLifecycleStore{}, Runs: agenttest.NoRuns{}, Decisions: agenttest.NoDecisions{}, Logger: logger,
		}),
		Persist: testWriter(t, messages),
	})
}

// driveUnrouted runs a resumed run the way a caller that follows it on eventCh
// does, and returns the failure it would be handed.
func driveUnrouted(t *testing.T, driver *drive.Driver, req runreq.ChatRequest, plan drive.RunPlan, eventCh chan runreq.WSStreamEvent) error {
	t.Helper()
	var delivery agentapi.RunDelivery = drive.NoDelivery{}
	if eventCh != nil {
		delivery = drive.NewChannelDelivery(eventCh)
	}
	result := driver.Drive(context.Background(), drive.RunSpec{
		Req:      req,
		Delivery: delivery,
		Input:    outcome.InputAccepted,
		Setup: func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
			return plan, nil
		},
	})
	if eventCh != nil {
		return nil
	}
	return drive.PublicRunFailure(result.Failure)
}

func resumedRequest() runreq.ChatRequest {
	return runreq.ChatRequest{
		RunID: agenttest.LifecycleRunID,
		BotID: agenttest.LifecycleBotID, ChatID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID,
		UserMessagePersisted: true, PersistedUserMessageID: "request-message",
	}
}

// failureRow is the one assistant failure row a resumed run left. A resumed run
// writes no user row: the turn's request message is in history.
func failureRow(t *testing.T, store *agenttest.RecordingStepPersister) messagepkg.PersistInput {
	t.Helper()
	if len(store.Steps) != 1 || !store.Steps[0].Interrupted || len(store.Steps[0].Messages) != 1 || store.Steps[0].Messages[0].Role != "assistant" {
		t.Fatalf("persisted steps %#v, want one interrupted step with an assistant failure row", store.Steps)
	}
	row := store.Steps[0].Messages[0]
	if row.TurnRequestMessageID != "request-message" {
		t.Fatalf("failure row turn request = %q, want the parked turn's request message", row.TurnRequestMessageID)
	}
	return row
}

// rejectingProvider answers every model call with the provider's refusal.
type rejectingProvider struct{}

func (rejectingProvider) Name() string                                    { return "rejecting" }
func (rejectingProvider) ListModels(context.Context) ([]sdk.Model, error) { return nil, nil }
func (rejectingProvider) Test(context.Context) error                      { return nil }
func (rejectingProvider) TestModel(context.Context, string) (*sdk.ModelTestResult, error) {
	return &sdk.ModelTestResult{Supported: true}, nil
}

func (rejectingProvider) DoGenerate(context.Context, sdk.Request) (sdk.ModelResult, error) {
	return sdk.ModelResult{}, &sdk.APIError{Provider: "openai-completions", StatusCode: 401, Kind: sdk.KindAuthentication, Message: "SECRET key"}
}

func (rejectingProvider) DoStream(context.Context, sdk.Request) (<-chan sdk.StreamPart, error) {
	return nil, &sdk.APIError{Provider: "openai-completions", StatusCode: 401, Kind: sdk.KindAuthentication, Message: "SECRET key"}
}

// A resumed run whose model call the provider refuses before its first step
// ends under the provider's code everywhere the failure is reported: the
// caller's error event with the catalog detail, the run's failure, and the
// failure row. The provider's own words reach none of them.
func TestResumedRunNamesTheProviderFailure(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingStepPersister{RecordingMessages: &agenttest.RecordingMessages{}}
	req := resumedRequest()
	cfg := native.RunConfig{
		Model:    &sdk.Model{ID: "continuation-model", Provider: rejectingProvider{}, Type: sdk.ModelTypeChat},
		Messages: []sdk.Message{sdk.UserMessage("continue after the decision")},
		Identity: native.SessionContext{BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID},
	}
	exec := &nativeExecutor{
		agent:     native.New(native.Deps{Logger: slog.New(slog.DiscardHandler)}),
		cfg:       cfg,
		rc:        runreq.ResolvedContext{RunConfig: cfg, Model: models.GetResponse{ID: "model-1"}},
		committer: testCommitter(t, messages, req),
	}
	eventCh := make(chan runreq.WSStreamEvent, 64)

	err := driveUnrouted(t, testDriver(t, messages), req, drive.RunPlan{Exec: exec, Req: req}, eventCh)
	close(eventCh)
	if err != nil {
		t.Fatalf("continuation error = %v, want the failure delivered in the stream", err)
	}

	var errorEvents []native.StreamEvent
	for raw := range eventCh {
		if strings.Contains(string(raw), "SECRET") {
			t.Fatalf("delivered event leaked the cause: %s", raw)
		}
		var ev native.StreamEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("decode delivered event %s: %v", raw, err)
		}
		if ev.Type == native.EventError {
			errorEvents = append(errorEvents, ev)
		}
	}
	definition, _ := apperror.Lookup(apperror.CodeAgentProviderAuthFailed)
	if len(errorEvents) != 1 || errorEvents[0].Code != string(apperror.CodeAgentProviderAuthFailed) || errorEvents[0].Error != definition.Detail {
		t.Fatalf("delivered error events = %#v, want one with the provider's code and detail", errorEvents)
	}
	row := failureRow(t, messages)
	if got := row.Metadata[messagepkg.HistoryErrorCodeMetadataKey]; got != string(apperror.CodeAgentProviderAuthFailed) {
		t.Fatalf("failure row code = %#v, want %s", got, apperror.CodeAgentProviderAuthFailed)
	}
}

// A failed run whose terminal still carries the conversation it was given, but
// nothing the user saw, leaves one failure row: the row is the failure's, and
// the terminal's messages have no output of their own to add.
func TestResumedRunTerminalWithoutOutputWritesOneRow(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingStepPersister{RecordingMessages: &agenttest.RecordingMessages{}}
	snapshot, err := json.Marshal([]sdk.Message{sdk.UserMessage("continue after the decision")})
	if err != nil {
		t.Fatal(err)
	}
	failure := apperror.Wrap(apperror.CodeAgentProviderAuthFailed, errors.New("SECRET key"), nil)
	req := resumedRequest()
	exec := &nativeExecutor{
		agent: fixedStream{
			{Type: native.EventAgentStart},
			{Type: native.EventError, Code: string(apperror.CodeAgentProviderAuthFailed), Cause: failure},
			{Type: native.EventAgentAbort, Messages: snapshot, EndReason: event.EndFailed},
		},
		committer: testCommitter(t, messages, req),
	}

	_ = driveUnrouted(t, testDriver(t, messages), req, drive.RunPlan{Exec: exec, Req: req}, nil)

	row := failureRow(t, messages)
	if got := row.Metadata[messagepkg.HistoryErrorCodeMetadataKey]; got != string(apperror.CodeAgentProviderAuthFailed) {
		t.Fatalf("failure row code = %#v, want %s", got, apperror.CodeAgentProviderAuthFailed)
	}
}

// A run that ends cleanly without anything the user could see has nothing to
// write: it commits without error and leaves no synthetic row.
func TestNativeExecutorCommitWithoutOutputWritesNothing(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingStepPersister{RecordingMessages: &agenttest.RecordingMessages{}}
	exec := &nativeExecutor{
		agent:     fixedStream{{Type: native.EventAgentStart}, {Type: native.EventAgentEnd}},
		committer: testCommitter(t, messages, resumedRequest()),
	}

	ending := exec.Execute(context.Background(), func(native.StreamEvent) {})
	if _, err := ending.Commit(context.Background(), nil, false); err != nil {
		t.Fatalf("Commit() error = %v, want a clean commit", err)
	}
	if len(messages.Steps) != 0 || len(messages.Persisted) != 0 {
		t.Fatalf("persisted steps %#v rows %#v, want nothing without output", messages.Steps, messages.Persisted)
	}
}

// commitStreamer is a native runtime that commits one step through the run's
// configured barrier and reports the failure as the runtime does: an error
// event carrying the barrier's error, then an aborted terminal.
type commitStreamer struct {
	// ownershipLost commits the step under a context whose ownership was
	// revoked.
	ownershipLost bool
}

func (s commitStreamer) Stream(ctx context.Context, cfg native.RunConfig) <-chan native.StreamEvent {
	if s.ownershipLost {
		var cancel context.CancelCauseFunc
		ctx, cancel = context.WithCancelCause(ctx)
		cancel(sessionruntime.ErrRunOwnershipLost)
	}
	ch := make(chan native.StreamEvent, 8)
	ch <- native.StreamEvent{Type: native.EventAgentStart}
	record := &step.Record{Messages: []sdk.Message{sdk.AssistantMessage("an answer")}}
	if _, err := cfg.OnStepCommitted(ctx, 0, record); err != nil {
		ch <- native.StreamEvent{Type: native.EventError, Code: string(apperror.CodeRuntimeRunFailed), Cause: fmt.Errorf("commit step 0: %w", err)}
		ch <- native.StreamEvent{Type: native.EventAgentAbort, EndReason: event.EndFailed}
	} else {
		ch <- native.StreamEvent{Type: native.EventAgentEnd}
	}
	close(ch)
	return ch
}

func driveCommitStep(t *testing.T, messages *agenttest.RecordingStepPersister) (drive.RunResult, []native.StreamEvent) {
	t.Helper()
	req := resumedRequest()
	committer := testCommitter(t, messages, req)
	var cfg native.RunConfig
	persist.ConfigureNativeReasoningTiming(&cfg, persist.NewReasoningTimingTracker(time.Now), committer)
	exec := &nativeExecutor{agent: commitStreamer{}, cfg: cfg, committer: committer}
	eventCh := make(chan runreq.WSStreamEvent, 64)
	result := testDriver(t, messages).Drive(context.Background(), drive.RunSpec{
		Req:      req,
		Delivery: drive.NewChannelDelivery(eventCh),
		Input:    outcome.InputAccepted,
		Setup: func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
			return drive.RunPlan{Exec: exec, Req: req}, nil
		},
	})
	close(eventCh)
	var events []native.StreamEvent
	for raw := range eventCh {
		var ev native.StreamEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("decode delivered event %s: %v", raw, err)
		}
		events = append(events, ev)
	}
	return result, events
}

// A step the committer cannot write fails the run as a history write: one
// failure is reported under that code, no failure row is written over the
// history that could not be, and the terminal is not published.
func TestNativeStepWriteFailureEndsTheRunAsHistoryInconsistent(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingStepPersister{
		RecordingMessages: &agenttest.RecordingMessages{},
		StepErr:           errors.New("history unavailable"),
	}
	result, events := driveCommitStep(t, messages)

	if result.End != outcome.EndFailed || result.Failure == nil ||
		result.Failure.Code != apperror.CodeSessionHistoryInconsistent || result.Failure.Phase != outcome.PhaseCommit {
		t.Fatalf("result = %v %+v, want a failed commit phase named %s", result.End, result.Failure, apperror.CodeSessionHistoryInconsistent)
	}
	var failures int
	for _, ev := range events {
		if ev.IsTerminal() {
			t.Fatalf("published terminal %#v after a failed history write", ev)
		}
		if ev.Type == native.EventError {
			failures++
			if ev.Code != string(apperror.CodeSessionHistoryInconsistent) {
				t.Fatalf("error event code = %q, want %s", ev.Code, apperror.CodeSessionHistoryInconsistent)
			}
		}
	}
	if failures != 1 {
		t.Fatalf("error events = %d in %#v, want exactly one", failures, events)
	}
	if len(messages.Steps) != 0 {
		t.Fatalf("steps = %#v, want no failure row over a history that could not be written", messages.Steps)
	}
}

// A step refused because the run's ownership was revoked is not taken as a
// history failure: the runtime's failure event goes on to the run's failure
// policy, which names a lost run from the run's own context.
func TestNativeStepWriteAfterOwnershipLossIsNotAHistoryFailure(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingStepPersister{RecordingMessages: &agenttest.RecordingMessages{}}
	committer := testCommitter(t, messages, resumedRequest())
	var cfg native.RunConfig
	persist.ConfigureNativeReasoningTiming(&cfg, persist.NewReasoningTimingTracker(time.Now), committer)
	exec := &nativeExecutor{agent: commitStreamer{ownershipLost: true}, cfg: cfg, committer: committer}

	var emitted []native.StreamEvent
	ending := exec.Execute(context.Background(), func(ev native.StreamEvent) { emitted = append(emitted, ev) })

	if ending.Cause != nil || exec.stepErr != nil {
		t.Fatalf("ending cause = %v, want none for a lost run", ending.Cause)
	}
	var reported bool
	for _, ev := range emitted {
		reported = reported || ev.Type == native.EventError
	}
	if !reported {
		t.Fatalf("emitted %#v, want the runtime's failure event to pass through", emitted)
	}
}
