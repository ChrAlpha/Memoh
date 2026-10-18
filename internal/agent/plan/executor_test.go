package plan

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/models"
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

// A replacement run without step commits has no queue coordinator to finalize
// it: the ending writes the new turn from the terminal snapshot, and the
// replacement's PostPersist then rewrites the old turn, once.
func TestNativeReplacementWithoutStepCommitsRewritesTheTurnOnce(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	position := int64(2)
	req := runreq.ChatRequest{
		BotID: agenttest.LifecycleBotID, ChatID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID,
		UserMessagePersisted: true, PersistedUserMessageID: "request-message",
		ReusePersistedUserMessage: true, TurnID: "new-turn", TurnPosition: &position,
	}
	snapshot, err := json.Marshal([]sdk.Message{sdk.AssistantMessage("ANSWER")})
	if err != nil {
		t.Fatal(err)
	}
	var rewrites int
	var persistedAtRewrite int
	exec := &nativeExecutor{
		agent: fixedStream{
			{Type: native.EventAgentStart},
			{Type: native.EventTextDelta, Delta: "ANSWER"},
			{Type: native.EventAgentEnd, Messages: snapshot, EndReason: event.EndCompleted},
		},
		req:    req,
		timing: persist.NewReasoningTimingTracker(nil),
		replace: &drive.TurnReplacement{PostPersist: func(_ context.Context, persisted []messagepkg.Message) error {
			rewrites++
			persistedAtRewrite = len(persisted)
			return nil
		}},
		persist: testWriter(t, messages),
	}

	ending := exec.Execute(context.Background(), func(native.StreamEvent) {})
	if _, err := ending.Commit(context.Background(), nil, false); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if len(messages.Persisted) == 0 {
		t.Fatal("the terminal snapshot was not written")
	}
	if rewrites != 1 || persistedAtRewrite == 0 {
		t.Fatalf("PostPersist calls = %d with %d persisted messages, want one call after the new turn was written", rewrites, persistedAtRewrite)
	}
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
		timing:  persist.NewReasoningTimingTracker(nil),
		persist: testWriter(t, &agenttest.RecordingMessages{}),
	}

	ending := exec.Execute(context.Background(), func(native.StreamEvent) {})
	if ending.CommitStopped != nil {
		t.Fatal("the native ending offers a stopped write, want the runtime's own step checkpoint to be the only one")
	}
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
	var delivery drive.RunDelivery = drive.NoDelivery{}
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
		BotID: agenttest.LifecycleBotID, ChatID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID,
		UserMessagePersisted: true, PersistedUserMessageID: "request-message",
	}
}

// failureRow is the one assistant failure row a resumed run left. A resumed run
// writes no user row: the turn's request message is in history.
func failureRow(t *testing.T, messages *agenttest.RecordingMessages) messagepkg.PersistInput {
	t.Helper()
	if len(messages.Persisted) != 1 || messages.Persisted[0].Role != "assistant" {
		t.Fatalf("persisted %#v, want one assistant failure row", messages.Persisted)
	}
	row := messages.Persisted[0]
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

	messages := &agenttest.RecordingMessages{}
	req := resumedRequest()
	cfg := native.RunConfig{
		Model:    &sdk.Model{ID: "continuation-model", Provider: rejectingProvider{}, Type: sdk.ModelTypeChat},
		Messages: []sdk.Message{sdk.UserMessage("continue after the decision")},
		Identity: native.SessionContext{BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID},
	}
	exec := &nativeExecutor{
		agent:   native.New(native.Deps{Logger: slog.New(slog.DiscardHandler)}),
		cfg:     cfg,
		rc:      runreq.ResolvedContext{RunConfig: cfg, Model: models.GetResponse{ID: "model-1"}},
		req:     req,
		timing:  persist.NewReasoningTimingTracker(nil),
		persist: testWriter(t, messages),
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
// the terminal's snapshot has no output of its own to add.
func TestResumedRunSnapshotWithoutOutputWritesOneRow(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
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
		req:     req,
		timing:  persist.NewReasoningTimingTracker(nil),
		persist: testWriter(t, messages),
	}

	_ = driveUnrouted(t, testDriver(t, messages), req, drive.RunPlan{Exec: exec, Req: req}, nil)

	row := failureRow(t, messages)
	if got := row.Metadata[messagepkg.HistoryErrorCodeMetadataKey]; got != string(apperror.CodeAgentProviderAuthFailed) {
		t.Fatalf("failure row code = %#v, want %s", got, apperror.CodeAgentProviderAuthFailed)
	}
}

// A run that ends cleanly without a snapshot and without anything the user
// could see has nothing to write: it commits without error and leaves no
// synthetic row. A run that showed output but has no snapshot to write it from
// is a history failure, not a success.
func TestNativeExecutorCommitWithoutASnapshot(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		stream  fixedStream
		wantErr bool
	}{
		{
			name:   "clean end with no output",
			stream: fixedStream{{Type: native.EventAgentStart}, {Type: native.EventAgentEnd}},
		},
		{
			name:    "visible output but no snapshot",
			stream:  fixedStream{{Type: native.EventAgentStart}, {Type: native.EventTextDelta, Delta: "partial"}, {Type: native.EventAgentEnd}},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			messages := &agenttest.RecordingMessages{}
			exec := &nativeExecutor{
				agent:   tc.stream,
				timing:  persist.NewReasoningTimingTracker(nil),
				persist: testWriter(t, messages),
			}

			ending := exec.Execute(context.Background(), func(native.StreamEvent) {})
			_, err := ending.Commit(context.Background(), nil, false)

			if tc.wantErr {
				if err == nil || apperror.CodeOf(err) != apperror.CodeSessionHistoryInconsistent {
					t.Fatalf("Commit() error = %v, want a history failure", err)
				}
			} else if err != nil {
				t.Fatalf("Commit() error = %v, want a clean commit", err)
			}
			if len(messages.Persisted) != 0 {
				t.Fatalf("persisted %#v, want no row without a snapshot", messages.Persisted)
			}
		})
	}
}
