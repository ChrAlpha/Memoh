package persist

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// History exit: a failure without assistant output stores the user message and
// an empty assistant message carrying the failure marker.
func TestTerminalSnapshotWithoutOutputWritesFailureMarker(t *testing.T) {
	t.Parallel()
	for _, code := range []apperror.Code{apperror.CodeAgentProviderOverloaded, apperror.CodeAgentResponseInterrupted} {
		messages := &agenttest.RecordingMessages{}
		writer := newTestWriter(Deps{Messages: messages, Sessions: agenttest.NewChatSessions()})
		if _, err := writer.TerminalSnapshotResult(context.Background(),
			runreq.ChatRequest{BotID: "bot-1", ThreadID: "session-1", Query: "hello"},
			runreq.ResolvedContext{}, TerminalSnapshot{Failure: outcome.RunFailure{Code: code}}); err != nil {
			t.Fatalf("persistTerminalSnapshotResult: %v", err)
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

// History exit with visible partial output: the partial answer is stored as an
// interrupted row carrying the run's failure code and args, in the shape the
// step path writes (see StepCommitter.RecordFailure). No separate failure row
// is written and the error text stays out of metadata.
func TestTerminalSnapshotWithPartialOutputKeepsTheFailureCode(t *testing.T) {
	t.Parallel()
	failure := outcome.RunFailure{
		Code:  apperror.CodeAgentProviderOverloaded,
		Args:  map[string]string{"provider": "acme"},
		Cause: errors.New("upstream said: secret detail"),
	}
	messages := &agenttest.RecordingMessages{}
	writer := newTestWriter(Deps{Messages: messages, Sessions: agenttest.NewChatSessions()})
	partial := TerminalSnapshot{
		SDKMessages:   []sdk.Message{sdk.AssistantMessage("partial answer")},
		VisibleOutput: true,
		Failure:       failure,
	}
	if _, err := writer.TerminalSnapshotResult(context.Background(),
		runreq.ChatRequest{BotID: "bot-1", ThreadID: "session-1", Query: "hello"}, runreq.ResolvedContext{}, partial); err != nil {
		t.Fatalf("TerminalSnapshotResult: %v", err)
	}

	if len(messages.Persisted) != 2 || messages.Persisted[0].Role != "user" || messages.Persisted[1].Role != "assistant" {
		t.Fatalf("persisted = %#v, want user + the partial assistant answer and no failure row", messages.Persisted)
	}
	want := map[string]any{
		messagepkg.AgentStepInterruptedMetadataKey: true,
		messagepkg.HistoryErrorCodeMetadataKey:     string(apperror.CodeAgentProviderOverloaded),
		messagepkg.HistoryErrorArgsMetadataKey:     map[string]string{"provider": "acme"},
	}
	if got := messages.Persisted[1].Metadata; !reflect.DeepEqual(got, want) {
		t.Fatalf("assistant metadata = %#v, want %#v", got, want)
	}
	if !strings.Contains(string(messages.Persisted[1].Content), "partial answer") {
		t.Fatalf("assistant content = %s, want the partial answer", messages.Persisted[1].Content)
	}

	// Without visible output the same snapshot becomes the failure marker.
	hidden := &agenttest.RecordingMessages{}
	writer = newTestWriter(Deps{Messages: hidden, Sessions: agenttest.NewChatSessions()})
	partial.VisibleOutput = false
	if _, err := writer.TerminalSnapshotResult(context.Background(),
		runreq.ChatRequest{BotID: "bot-1", ThreadID: "session-1", Query: "hello"}, runreq.ResolvedContext{}, partial); err != nil {
		t.Fatalf("TerminalSnapshotResult: %v", err)
	}
	if len(hidden.Persisted) != 2 || hidden.Persisted[1].Metadata["error_code"] != "agent.provider_overloaded" {
		t.Fatalf("persisted without visible output = %#v, want failure marker", hidden.Persisted)
	}
}

// Only the last assistant row of a snapshot is the interrupted answer; rows of
// earlier model calls completed and carry no failure.
func TestTerminalSnapshotFailureMarksOnlyTheLastAssistantRow(t *testing.T) {
	t.Parallel()
	messages := &agenttest.RecordingMessages{}
	writer := newTestWriter(Deps{Messages: messages, Sessions: agenttest.NewChatSessions()})
	snap := TerminalSnapshot{
		SDKMessages: []sdk.Message{
			sdk.AssistantMessage("first answer"),
			sdk.UserMessage("follow-up"),
			sdk.AssistantMessage("cut off"),
		},
		VisibleOutput: true,
		Failure:       outcome.RunFailure{Code: apperror.CodeAgentResponseInterrupted},
	}
	// The request's user message is already stored, so the round's own user
	// message is dropped before the rows are written.
	req := runreq.ChatRequest{BotID: "bot-1", ThreadID: "session-1", Query: "hello", UserMessagePersisted: true}
	if _, err := writer.TerminalSnapshotResult(context.Background(), req, runreq.ResolvedContext{}, snap); err != nil {
		t.Fatalf("TerminalSnapshotResult: %v", err)
	}
	var assistants []messagepkg.PersistInput
	for _, row := range messages.Persisted {
		if row.Role == "assistant" {
			assistants = append(assistants, row)
		}
	}
	if len(assistants) != 2 {
		t.Fatalf("persisted = %#v, want two assistant rows", messages.Persisted)
	}
	if _, has := assistants[0].Metadata[messagepkg.HistoryErrorCodeMetadataKey]; has {
		t.Fatalf("first assistant metadata = %#v, want no failure", assistants[0].Metadata)
	}
	if assistants[1].Metadata[messagepkg.HistoryErrorCodeMetadataKey] != string(apperror.CodeAgentResponseInterrupted) ||
		assistants[1].Metadata[messagepkg.AgentStepInterruptedMetadataKey] != true {
		t.Fatalf("last assistant metadata = %#v, want interrupted with the failure code", assistants[1].Metadata)
	}
}

// A turn the user stopped after partial output keeps the answer and carries no
// failure code: stop beats failure.
func TestTerminalSnapshotAbortedAfterPartialOutputKeepsTheAnswerWithoutCode(t *testing.T) {
	t.Parallel()
	messages := &agenttest.RecordingMessages{}
	writer := newTestWriter(Deps{Messages: messages, Sessions: agenttest.NewChatSessions()})
	snap := TerminalSnapshot{
		SDKMessages:   []sdk.Message{sdk.AssistantMessage("partial answer")},
		VisibleOutput: true,
		aborted:       true,
	}
	if _, err := writer.TerminalSnapshotResult(context.Background(),
		runreq.ChatRequest{BotID: "bot-1", ThreadID: "session-1", Query: "hello"}, runreq.ResolvedContext{}, snap); err != nil {
		t.Fatalf("TerminalSnapshotResult: %v", err)
	}
	if len(messages.Persisted) != 2 || messages.Persisted[1].Role != "assistant" {
		t.Fatalf("persisted = %#v, want user + the partial assistant answer", messages.Persisted)
	}
	metadata := messages.Persisted[1].Metadata
	for _, key := range []string{messagepkg.HistoryErrorCodeMetadataKey, messagepkg.HistoryErrorArgsMetadataKey, messagepkg.AgentStepInterruptedMetadataKey} {
		if _, has := metadata[key]; has {
			t.Fatalf("assistant metadata = %#v, an aborted turn carries no %s", metadata, key)
		}
	}
}
