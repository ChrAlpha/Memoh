package persist

import (
	"context"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

func TestPersistTurnFailureStartsRetryReplacementAtomically(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	resolver := newTestWriter(Deps{Messages: messages})

	persisted, err := resolver.persistTurnFailure(
		context.Background(),
		runreq.ChatRequest{
			BotID: "bot-1", ThreadID: "session-1", Query: "hello",
			ReusePersistedUserMessage: true, PersistedUserMessageID: "old-request",
			TurnReplacement: &messagepkg.TurnReplacement{OldTurnID: "old-turn", ReplacementTurnID: "new-turn", ReplacementTurnPosition: int64Pointer(2), RequestMessageID: "old-request"},
		},
		runreq.ResolvedContext{},
		apperror.CodeAgentResponseTimeout,
	)
	if err != nil {
		t.Fatalf("persistTurnFailure returned error: %v", err)
	}
	if len(persisted) != 2 || len(messages.Persisted) != 1 || len(messages.RoundOptions) != 1 || messages.RoundOptions[0].Replacement == nil {
		t.Fatalf("retry failure was not persisted atomically with its cloned user: %#v %#v", persisted, messages.RoundOptions)
	}
	if got := messages.Persisted[0]; got.SkipHistoryTurn || got.Metadata[messagepkg.HistoryErrorCodeMetadataKey] != string(apperror.CodeAgentResponseTimeout) {
		t.Fatalf("retry failure lost binding or stable code: %#v", got)
	}
}

func TestPersistTerminalSnapshotSkipsUserOnlySnapshot(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	resolver := newTestWriter(Deps{Messages: messages})

	if _, err := resolver.TerminalSnapshotResult(
		context.Background(),
		runreq.ChatRequest{
			BotID:    "bot-1",
			ThreadID: "session-1",
			Query:    "hello",
		},
		runreq.ResolvedContext{},
		TerminalSnapshot{
			SDKMessages: []sdk.Message{sdk.UserMessage("hello")},
		},
	); err != nil {
		t.Fatalf("persistTerminalSnapshot returned error: %v", err)
	}

	if len(messages.Persisted) != 0 {
		t.Fatalf("expected user-only terminal snapshot not to persist, got %#v", messages.Persisted)
	}
}

func TestPersistTerminalSnapshotSkipsEmptyAssistantSnapshot(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	resolver := newTestWriter(Deps{Messages: messages})

	if _, err := resolver.TerminalSnapshotResult(
		context.Background(),
		runreq.ChatRequest{
			BotID:    "bot-1",
			ThreadID: "session-1",
			Query:    "hello",
		},
		runreq.ResolvedContext{},
		TerminalSnapshot{
			SDKMessages: []sdk.Message{sdk.AssistantMessage("")},
		},
	); err != nil {
		t.Fatalf("persistTerminalSnapshot returned error: %v", err)
	}

	if len(messages.Persisted) != 0 {
		t.Fatalf("expected empty assistant terminal snapshot not to persist, got %#v", messages.Persisted)
	}
}

func TestPersistTerminalSnapshotSkipsAbortedSnapshotBeforeVisibleOutput(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	resolver := newTestWriter(Deps{Messages: messages})

	if _, err := resolver.TerminalSnapshotResult(
		context.Background(),
		runreq.ChatRequest{
			BotID:    "bot-1",
			ThreadID: "session-1",
			Query:    "hello",
		},
		runreq.ResolvedContext{},
		TerminalSnapshot{
			SDKMessages: []sdk.Message{sdk.AssistantMessage("partial answer")},
			aborted:     true,
		},
	); err != nil {
		t.Fatalf("persistTerminalSnapshot returned error: %v", err)
	}

	if len(messages.Persisted) != 0 {
		t.Fatalf("expected pre-output abort not to persist, got %#v", messages.Persisted)
	}
}

func TestPersistTerminalSnapshotStoresTimeoutBeforeVisibleOutput(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	resolver := newTestWriter(Deps{Messages: messages})

	if _, err := resolver.TerminalSnapshotResult(
		context.Background(),
		runreq.ChatRequest{
			BotID:    "bot-1",
			ThreadID: "session-1",
			Query:    "hello",
		},
		runreq.ResolvedContext{},
		TerminalSnapshot{
			aborted:     true,
			FailureCode: apperror.CodeAgentResponseTimeout,
		},
	); err != nil {
		t.Fatalf("persistTerminalSnapshot returned error: %v", err)
	}

	if len(messages.Persisted) != 2 {
		t.Fatalf("expected user + timeout assistant, got %#v", messages.Persisted)
	}
	if messages.Persisted[0].Role != "user" || messages.Persisted[1].Role != "assistant" {
		t.Fatalf("persisted roles = %s/%s", messages.Persisted[0].Role, messages.Persisted[1].Role)
	}
	if got, _ := messages.Persisted[1].Metadata[messagepkg.HistoryErrorCodeMetadataKey].(string); got != string(apperror.CodeAgentResponseTimeout) {
		t.Fatalf("error_code = %q, want %q", got, apperror.CodeAgentResponseTimeout)
	}
	if messages.Persisted[1].Metadata[messagepkg.AgentStepInterruptedMetadataKey] != true {
		t.Fatalf("expected interrupted metadata on timeout assistant")
	}
}

func TestPersistTerminalSnapshotStoresAssistantOutput(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	resolver := newTestWriter(Deps{Messages: messages})

	if _, err := resolver.TerminalSnapshotResult(
		context.Background(),
		runreq.ChatRequest{
			BotID:    "bot-1",
			ThreadID: "session-1",
			Query:    "hello",
		},
		runreq.ResolvedContext{},
		TerminalSnapshot{
			SDKMessages: []sdk.Message{sdk.AssistantMessage("partial answer")},
		},
	); err != nil {
		t.Fatalf("persistTerminalSnapshot returned error: %v", err)
	}

	if len(messages.Persisted) != 2 {
		t.Fatalf("expected user and assistant messages to persist, got %#v", messages.Persisted)
	}
	if messages.Persisted[0].Role != "user" || messages.Persisted[1].Role != "assistant" {
		t.Fatalf("unexpected persisted roles: %q, %q", messages.Persisted[0].Role, messages.Persisted[1].Role)
	}
}

func TestPersistTerminalSnapshotPersistsUserWhenPipelineContextContainsCurrentMessage(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	resolver := newTestWriter(Deps{Messages: messages})

	if _, err := resolver.TerminalSnapshotResult(
		context.Background(),
		runreq.ChatRequest{
			BotID:    "bot-1",
			ThreadID: "session-1",
			Query:    "---\nmessage-id: tg-1\nchannel: telegram\n---\n@memoh1bot ping",
		},
		runreq.ResolvedContext{},
		TerminalSnapshot{
			SDKMessages: []sdk.Message{sdk.AssistantMessage("pong")},
		},
	); err != nil {
		t.Fatalf("persistTerminalSnapshot returned error: %v", err)
	}

	if len(messages.Persisted) != 2 {
		t.Fatalf("expected user and assistant output to persist, got %#v", messages.Persisted)
	}
	if messages.Persisted[0].Role != "user" {
		t.Fatalf("unexpected first persisted role: %q", messages.Persisted[0].Role)
	}
	if messages.Persisted[1].Role != "assistant" {
		t.Fatalf("unexpected second persisted role: %q", messages.Persisted[1].Role)
	}
}

func TestPersistTerminalSnapshotSkillActivationWithoutPromptDoesNotStoreModelMarker(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	resolver := newTestWriter(Deps{Messages: messages})
	activation := &runreq.SkillActivation{
		Skills: []runreq.SkillActivationSkill{{Name: "alpha", DisplayName: "Alpha", State: "effective"}},
	}
	req := runreq.ChatRequest{
		BotID:                "bot-1",
		ThreadID:             "session-1",
		ModelQuery:           turn.SkillActivationModelQuery(activation),
		UserMessageKind:      runreq.UserMessageKindSkillActivation,
		SkillActivation:      activation,
		SkipMemoryExtraction: true,
	}

	if _, err := resolver.TerminalSnapshotResult(
		context.Background(),
		req,
		runreq.ResolvedContext{},
		TerminalSnapshot{
			SDKMessages:   []sdk.Message{sdk.AssistantMessage("done")},
			VisibleOutput: true,
		},
	); err != nil {
		t.Fatalf("persistTerminalSnapshot returned error: %v", err)
	}

	if len(messages.Persisted) != 2 {
		t.Fatalf("persisted messages = %d, want user + assistant", len(messages.Persisted))
	}
	user := messages.Persisted[0]
	if user.Role != "user" {
		t.Fatalf("first persisted role = %q, want user", user.Role)
	}
	if got := persistedTextContent(t, user.Content); got != "" {
		t.Fatalf("persisted user content = %q, want empty", got)
	}
	if user.DisplayText != "" {
		t.Fatalf("display text = %q, want empty", user.DisplayText)
	}
	if user.Metadata["user_message_kind"] != runreq.UserMessageKindSkillActivation {
		t.Fatalf("metadata kind = %#v, want skill_activation", user.Metadata["user_message_kind"])
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
