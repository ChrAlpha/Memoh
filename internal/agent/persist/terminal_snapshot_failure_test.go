package persist

import (
	"context"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
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
			runreq.ResolvedContext{}, TerminalSnapshot{FailureCode: code}); err != nil {
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

// History exit with visible partial output: the partial answer is stored and
// the failure code is not.
//
// Current behavior: a turn that failed after printing text looks like a normal
// assistant message in history.
func TestCharacterizeHistoryPartialOutputDropsFailureCode_CurrentBehavior(t *testing.T) {
	t.Parallel()
	messages := &agenttest.RecordingMessages{}
	writer := newTestWriter(Deps{Messages: messages, Sessions: agenttest.NewChatSessions()})
	partial := TerminalSnapshot{
		SDKMessages:   []sdk.Message{sdk.AssistantMessage("partial answer")},
		VisibleOutput: true,
		FailureCode:   apperror.CodeAgentProviderOverloaded,
	}
	if _, err := writer.TerminalSnapshotResult(context.Background(),
		runreq.ChatRequest{BotID: "bot-1", ThreadID: "session-1", Query: "hello"}, runreq.ResolvedContext{}, partial); err != nil {
		t.Fatalf("persistTerminalSnapshotResult: %v", err)
	}

	if len(messages.Persisted) != 2 {
		t.Fatalf("persisted = %#v, want user + assistant", messages.Persisted)
	}
	metadata := messages.Persisted[1].Metadata
	if _, has := metadata[messagepkg.HistoryErrorCodeMetadataKey]; has {
		t.Fatalf("assistant metadata = %#v, current behavior stores no error_code", metadata)
	}

	// Without visible output the same partial snapshot becomes the failure marker.
	hidden := &agenttest.RecordingMessages{}
	writer = newTestWriter(Deps{Messages: hidden, Sessions: agenttest.NewChatSessions()})
	partial.VisibleOutput = false
	if _, err := writer.TerminalSnapshotResult(context.Background(),
		runreq.ChatRequest{BotID: "bot-1", ThreadID: "session-1", Query: "hello"}, runreq.ResolvedContext{}, partial); err != nil {
		t.Fatalf("persistTerminalSnapshotResult: %v", err)
	}
	if len(hidden.Persisted) != 2 || hidden.Persisted[1].Metadata["error_code"] != "agent.provider_overloaded" {
		t.Fatalf("persisted without visible output = %#v, want failure marker", hidden.Persisted)
	}
}
