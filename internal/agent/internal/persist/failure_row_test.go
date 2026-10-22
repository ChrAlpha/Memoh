package persist

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/outcome"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

func TestFailureRowStartsRetryReplacementAtomically(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	resolver := newTestWriter(Deps{Messages: messages})

	persisted, err := resolver.FailureRow(
		context.Background(),
		runreq.ChatRequest{
			BotID: "bot-1", ThreadID: "session-1", Query: "hello",
			ReusePersistedUserMessage: true, PersistedUserMessageID: "old-request",
			TurnReplacement: &messagepkg.TurnReplacement{OldTurnID: "old-turn", ReplacementTurnID: "new-turn", ReplacementTurnPosition: int64Pointer(2), RequestMessageID: "old-request"},
		},
		runreq.ResolvedContext{},
		outcome.RunFailure{Code: apperror.CodeAgentResponseTimeout},
		"",
	)
	if err != nil {
		t.Fatalf("FailureRow returned error: %v", err)
	}
	if len(persisted) != 2 || len(messages.Persisted) != 1 || len(messages.RoundOptions) != 1 || messages.RoundOptions[0].Replacement == nil {
		t.Fatalf("retry failure was not persisted atomically with its cloned user: %#v %#v", persisted, messages.RoundOptions)
	}
	if got := messages.Persisted[0]; got.SkipHistoryTurn || got.Metadata[messagepkg.HistoryErrorCodeMetadataKey] != string(apperror.CodeAgentResponseTimeout) {
		t.Fatalf("retry failure lost binding or stable code: %#v", got)
	}
}

// A Web send the user-message hook refused leaves its two rows under the turn
// admission named, both marked with the hook origin.
func TestHookFailureRowUsesAdmittedIdentity(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	service := newTestWriter(Deps{Messages: messages})
	position := int64(7)
	_, err := service.FailureRow(context.Background(), runreq.ChatRequest{
		BotID:        "bot-1",
		ThreadID:     "session-1",
		RunID:        "run-1",
		TurnID:       "turn-1",
		TurnPosition: &position,
		Query:        "hello",
	}, runreq.ResolvedContext{}, outcome.RunFailure{Code: apperror.CodeHookUserMessageFailed}, messagepkg.HistoryFailureOriginUserMessageHook)
	if err != nil {
		t.Fatalf("persistFailureRow: %v", err)
	}
	if len(messages.Persisted) != 2 {
		t.Fatalf("persisted %d messages, want user + assistant", len(messages.Persisted))
	}
	for i, input := range messages.Persisted {
		if i == 0 && (input.TurnID != "turn-1" || input.TurnPosition == nil || *input.TurnPosition != position) {
			t.Fatalf("user identity = (%q, %v), want (turn-1, %d)", input.TurnID, input.TurnPosition, position)
		}
		if input.Metadata[messagepkg.HistoryFailureOriginMetadataKey] != messagepkg.HistoryFailureOriginUserMessageHook {
			t.Fatalf("message %d metadata = %#v, want Hook failure origin", i, input.Metadata)
		}
		if i == 1 && input.Metadata[messagepkg.HistoryErrorCodeMetadataKey] != string(apperror.CodeHookUserMessageFailed) {
			t.Fatalf("assistant metadata = %#v, want Hook failure code", input.Metadata)
		}
	}
}

// A failure without assistant output stores the user message and an empty
// assistant message carrying the failure marker.
func TestFailureRowWithoutOutputWritesFailureMarker(t *testing.T) {
	t.Parallel()
	for _, code := range []apperror.Code{apperror.CodeAgentProviderOverloaded, apperror.CodeAgentResponseInterrupted} {
		messages := &agenttest.RecordingMessages{}
		writer := newTestWriter(Deps{Messages: messages, Sessions: agenttest.NewChatSessions()})
		if _, err := writer.FailureRow(context.Background(),
			runreq.ChatRequest{BotID: "bot-1", ThreadID: "session-1", Query: "hello"},
			runreq.ResolvedContext{}, outcome.RunFailure{Code: code}, ""); err != nil {
			t.Fatalf("FailureRow: %v", err)
		}
		if len(messages.Persisted) != 2 || messages.Persisted[0].Role != "user" || messages.Persisted[1].Role != "assistant" {
			t.Fatalf("persisted = %#v, want user + assistant", messages.Persisted)
		}
		metadata := messages.Persisted[1].Metadata
		if metadata["agent_step_interrupted"] != true || metadata["error_code"] != string(code) {
			t.Fatalf("assistant metadata = %#v, want interrupted with error_code %q", metadata, code)
		}
		if _, hasError := metadata["error"]; hasError {
			t.Fatalf("assistant metadata = %#v, native failure has no error text key", metadata)
		}
	}
}

// A failure after partial output: the step committer stores the partial answer
// as an interrupted row carrying the run's failure code and args. The error
// text stays out of metadata.
func TestStepCommitterRecordFailureKeepsThePartialAnswerWithCodeAndArgs(t *testing.T) {
	fx := newSteerFixture(t)
	handle := fx.handle
	ctx := replacementTestContext(fx)
	committer := fx.writer.NewStepCommitter(ctx, runreq.ChatRequest{
		BotID: handle.BotID, ThreadID: handle.SessionID, RunID: handle.RunID, RunHandle: handle,
		TurnID: "turn-1", TurnPosition: int64Pointer(1), Query: "hello",
	}, replacementResolvedContext())
	if committer == nil {
		t.Fatal("missing step committer")
	}
	failure := outcome.RunFailure{
		Code:  apperror.CodeAgentProviderOverloaded,
		Args:  map[string]string{"provider": "acme"},
		Cause: errors.New("upstream said: secret detail"),
	}
	if _, err := committer.RecordFailure(ctx, "partial answer", failure); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	steps := fx.messages.Steps
	if len(steps) != 1 || !steps[0].Interrupted {
		t.Fatalf("steps = %#v, want one interrupted step", steps)
	}
	rows := steps[0].Messages
	if len(rows) != 2 || rows[0].Role != "user" || rows[1].Role != "assistant" {
		t.Fatalf("rows = %#v, want user + the partial assistant answer and no failure row", rows)
	}
	want := map[string]any{
		messagepkg.AgentStepInterruptedMetadataKey: true,
		messagepkg.HistoryErrorCodeMetadataKey:     string(apperror.CodeAgentProviderOverloaded),
		messagepkg.HistoryErrorArgsMetadataKey:     map[string]string{"provider": "acme"},
	}
	got := map[string]any{}
	for key := range want {
		if value, ok := rows[1].Metadata[key]; ok {
			got[key] = value
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("assistant failure metadata = %#v, want %#v", got, want)
	}
	if _, hasError := rows[1].Metadata["error"]; hasError {
		t.Fatalf("assistant metadata = %#v, the error text must stay out", rows[1].Metadata)
	}
	if !strings.Contains(string(rows[1].Content), "partial answer") {
		t.Fatalf("assistant content = %s, want the partial answer", rows[1].Content)
	}
}
