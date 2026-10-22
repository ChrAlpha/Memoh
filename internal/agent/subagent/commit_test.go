package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/agenttest"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/persist"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/step"
	"github.com/felinics/memoh/internal/agent/toolexec"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/runtimefence"
)

func runContext(botID, sessionID string, token int64) context.Context {
	ctx := runtimefence.WithContext(context.Background(), runtimefence.Fence{
		BotID: botID, SessionID: sessionID, Token: token,
	})
	return withHandle(ctx, sessionruntime.RunHandle{
		BotID:        botID,
		SessionID:    sessionID,
		RunID:        uuid.NewString(),
		TurnID:       uuid.NewString(),
		Generation:   "gen-1",
		FencingToken: token,
	})
}

func newHost(t *testing.T, store *agenttest.RecordingStepPersister) *Host {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	return New(Deps{Persist: persist.New(persist.Deps{
		Logger:   logger,
		Messages: store,
		Sessions: &agenttest.Sessions{GetFn: func(context.Context, string) (sessionpkg.Thread, error) {
			return sessionpkg.Thread{}, errors.New("session not found")
		}},
		Bot: agenttest.BotEnv(logger),
	})})
}

// The run's history has no other writer, so a run that cannot commit steps is an
// error rather than a run without history.
func TestSubagentStepCommitRequiresHandleFenceAndRequestRow(t *testing.T) {
	botID, sessionID := uuid.NewString(), uuid.NewString()
	store := &agenttest.RecordingStepPersister{RecordingMessages: &agenttest.RecordingMessages{}}
	service := newHost(t, store)

	refused := func(name string, ctx context.Context, requestID string) {
		t.Helper()
		if commit, interrupt, err := service.StepCommit(ctx, botID, sessionID, "model", requestID, nil, nil); err == nil || commit != nil || interrupt != nil {
			t.Fatalf("%s: commit enabled (err=%v)", name, err)
		}
	}
	refused("no admitted run handle", context.Background(), "req-msg-1")
	refused("no runtime fence", withHandle(context.Background(), sessionruntime.RunHandle{
		BotID: botID, SessionID: sessionID, RunID: uuid.NewString(), Generation: "g", FencingToken: 7,
	}), "req-msg-1")
	unfenced := runtimefence.WithContext(context.Background(), runtimefence.Fence{BotID: botID, SessionID: sessionID, Token: 7})
	refused("zero fencing token", withHandle(unfenced, sessionruntime.RunHandle{
		BotID: botID, SessionID: sessionID, RunID: uuid.NewString(), Generation: "g",
	}), "req-msg-1")
	refused("no request row", runContext(botID, sessionID, 7), "  ")
	if commit, interrupt, err := service.StepCommit(runContext(botID, sessionID, 7), botID, sessionID, "model", "req-msg-1", nil, nil); err != nil || commit == nil || interrupt == nil {
		t.Fatalf("commit not enabled for an admitted fenced subagent run: %v", err)
	}
}

func TestSubagentStepCommitPersistsStepsInOrder(t *testing.T) {
	botID, sessionID := uuid.NewString(), uuid.NewString()
	store := &agenttest.RecordingStepPersister{RecordingMessages: &agenttest.RecordingMessages{}}
	service := newHost(t, store)
	ctx := runContext(botID, sessionID, 9)
	holder := contextfrag.NewLifecycleHolder()
	holder.SetManifest(contextfrag.Manifest{
		View:   contextfrag.ViewRunConfigPreProvider,
		Counts: contextfrag.ManifestCounts{Fragments: 3, Messages: 2},
	})
	persistedSteps := 0
	commit, interrupt, err := service.StepCommit(ctx, botID, sessionID, "model-uuid", "req-msg-1", holder, func() { persistedSteps++ })
	if err != nil {
		t.Fatalf("StepCommit: %v", err)
	}

	if err := commit(ctx, 0, &step.Record{Messages: []sdk.Message{
		sdk.AssistantMessage("step one"),
	}}); err != nil {
		t.Fatalf("commit step 0: %v", err)
	}
	if err := commit(ctx, 1, &step.Record{Messages: []sdk.Message{sdk.AssistantMessage("step two")}}); err != nil {
		t.Fatalf("commit step 1: %v", err)
	}
	if err := commit(ctx, 3, &step.Record{Messages: []sdk.Message{sdk.AssistantMessage("skipped ahead")}}); err == nil {
		t.Fatal("out-of-order step index was accepted")
	}

	if len(store.Steps) != 2 {
		t.Fatalf("persisted %d steps, want 2", len(store.Steps))
	}
	first := store.Steps[0]
	if len(first.Messages) != 1 {
		t.Fatalf("step 0 persisted %d messages, want the assistant one only", len(first.Messages))
	}
	msg := first.Messages[0]
	if msg.Role != string(sdk.MessageRoleAssistant) || msg.BotID != botID || msg.SessionID != sessionID {
		t.Fatalf("unexpected persisted message: %+v", msg)
	}
	if msg.ModelID != "model-uuid" {
		t.Fatalf("model id = %q, want model-uuid", msg.ModelID)
	}
	if msg.RunID == "" || first.RunID != msg.RunID {
		t.Fatalf("run ids diverge: step %q message %q", first.RunID, msg.RunID)
	}
	if msg.TurnRequestMessageID != "req-msg-1" {
		t.Fatalf("step row does not bind to the task's request message: %q", msg.TurnRequestMessageID)
	}
	var decoded sdk.Message
	if err := json.Unmarshal(msg.Content, &decoded); err != nil {
		t.Fatalf("persisted content is not a marshalled sdk message: %v", err)
	}
	if persistedSteps != 2 {
		t.Fatalf("onPersisted fired %d times, want 2", persistedSteps)
	}
	firstLifecycle, ok := store.Steps[0].Messages[0].Metadata[contextfrag.MetadataContextLifecycleKey].(contextfrag.LifecycleSnapshot)
	if !ok || firstLifecycle.Counts.Fragments != 3 || firstLifecycle.AssistantMessageID != "" {
		t.Fatalf("first step lifecycle metadata = %#v, want pre-persist snapshot", store.Steps[0].Messages[0].Metadata)
	}
	secondLifecycle, ok := store.Steps[1].Messages[0].Metadata[contextfrag.MetadataContextLifecycleKey].(contextfrag.LifecycleSnapshot)
	if !ok || secondLifecycle.AssistantMessageID != "committed" {
		t.Fatalf("second step lifecycle metadata = %#v, want prior assistant association", store.Steps[1].Messages[0].Metadata)
	}
	associated, ok := holder.Snapshot()
	if !ok || associated.AssistantMessageID != "committed" {
		t.Fatalf("shared lifecycle snapshot = %#v, set = %v", associated, ok)
	}

	interrupted := sdk.Message{
		Role:    sdk.MessageRoleAssistant,
		Content: []sdk.MessagePart{sdk.ReasoningPart{Text: "partial inference"}},
	}
	if err := interrupt(ctx, 2, &step.Record{Messages: []sdk.Message{interrupted}}); err != nil {
		t.Fatalf("persist interrupted step: %v", err)
	}
	if len(store.Steps) != 3 || !store.Steps[2].Interrupted {
		t.Fatalf("interrupted step = %#v, want durable interrupted checkpoint", store.Steps)
	}
	if got := store.Steps[2].Messages[0].Metadata[messagepkg.AgentStepInterruptedMetadataKey]; got != true {
		t.Fatalf("interrupted metadata = %#v, want marker", store.Steps[2].Messages[0].Metadata)
	}
	if _, ok := store.Steps[2].Messages[0].Metadata[contextfrag.MetadataContextLifecycleKey].(contextfrag.LifecycleSnapshot); !ok {
		t.Fatalf("interrupted lifecycle metadata = %#v, want snapshot", store.Steps[2].Messages[0].Metadata)
	}
	if persistedSteps != 3 {
		t.Fatalf("interrupted checkpoint did not report durable output: %d", persistedSteps)
	}
}

func TestSubagentStepCommitSkipsEmptyStepWithoutPersisting(t *testing.T) {
	botID, sessionID := uuid.NewString(), uuid.NewString()
	store := &agenttest.RecordingStepPersister{RecordingMessages: &agenttest.RecordingMessages{}}
	service := newHost(t, store)
	ctx := runContext(botID, sessionID, 3)
	persistedSteps := 0
	commit, _, err := service.StepCommit(ctx, botID, sessionID, "model", "req-msg-1", nil, func() { persistedSteps++ })
	if err != nil {
		t.Fatalf("StepCommit: %v", err)
	}

	if err := commit(ctx, 0, &step.Record{}); err != nil {
		t.Fatalf("commit empty step: %v", err)
	}
	if len(store.Steps) != 0 || persistedSteps != 0 {
		t.Fatalf("empty step persisted: steps=%d fired=%d", len(store.Steps), persistedSteps)
	}
	// The empty step still advanced the cursor: the next step index is 1.
	if err := commit(ctx, 1, &step.Record{Messages: []sdk.Message{sdk.AssistantMessage("real output")}}); err != nil {
		t.Fatalf("commit step 1 after empty step: %v", err)
	}
	if len(store.Steps) != 1 || persistedSteps != 1 {
		t.Fatalf("follow-up step not persisted: steps=%d fired=%d", len(store.Steps), persistedSteps)
	}
}

func TestSubagentRejectsRevokedCheckpointWithDetachedCallback(t *testing.T) {
	store := &agenttest.RecordingStepPersister{RecordingMessages: &agenttest.RecordingMessages{}}
	service := newHost(t, store)
	owner, cancel := context.WithCancelCause(runContext("bot", "session", 9))
	_, checkpoint, err := service.StepCommit(owner, "bot", "session", "model", "request", nil, nil)
	if err != nil {
		t.Fatalf("StepCommit: %v", err)
	}
	cancel(sessionruntime.ErrRunOwnershipLost)
	err = checkpoint(context.WithoutCancel(owner), 0, &step.Record{Messages: []sdk.Message{sdk.AssistantMessage("late")}})
	if !errors.Is(err, sessionruntime.ErrRunOwnershipLost) || len(store.Steps) != 0 {
		t.Fatalf("revoked subagent checkpoint: writes=%d err=%v", len(store.Steps), err)
	}
}

// A tool call whose arguments cannot be encoded is typed as invalid argument
// text, so the message persists with that text instead of being dropped.
func TestSubagentStepCommitKeepsUnencodableInputAsText(t *testing.T) {
	botID, sessionID := uuid.NewString(), uuid.NewString()
	store := &agenttest.RecordingStepPersister{RecordingMessages: &agenttest.RecordingMessages{}}
	ctx := runContext(botID, sessionID, 4)
	commit, _, err := newHost(t, store).StepCommit(ctx, botID, sessionID, "model-1", "request-1", nil, nil)
	if err != nil {
		t.Fatalf("StepCommit: %v", err)
	}
	if err := commit(ctx, 0, &step.Record{Messages: []sdk.Message{
		{
			Role: sdk.MessageRoleAssistant,
			Content: []sdk.MessagePart{sdk.ToolCallPart{
				ToolCallID: "bad-call", ToolName: "bad", Input: toolexec.ArgumentsFromValue(func() {}),
			}},
		},
		sdk.AssistantMessage("durable report"),
	}}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if len(store.Steps) != 1 || len(store.Steps[0].Messages) != 2 {
		t.Fatalf("persisted steps = %#v, want one step with both assistant messages", store.Steps)
	}
	inputs := store.Steps[0].Messages
	if inputs[0].Role != string(sdk.MessageRoleAssistant) || inputs[1].Role != string(sdk.MessageRoleAssistant) {
		t.Fatalf("persisted inputs = %#v, want both assistant messages", inputs)
	}
	if !strings.Contains(string(inputs[0].Content), `"input":"0x`) {
		t.Fatalf("unencodable input = %s, want the printed value as invalid argument text", inputs[0].Content)
	}
}
