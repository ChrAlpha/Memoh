package persist

import (
	"context"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// A turn the user-message hook refused is written as one atomic round under
// the identity admission allocated: the user row sits in the admitted turn at
// the admitted position, and the assistant row carries the failure code. Both
// carry the hook origin, which is what lets history drop the pair together.
func TestFailureRowForAHookRefusalUsesTheAdmittedTurnInOneRound(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	writer := newTestWriter(Deps{Messages: messages})
	position := int64Pointer(7)

	persisted, err := writer.FailureRow(context.Background(), runreq.ChatRequest{
		BotID:        "bot-1",
		ThreadID:     "session-1",
		RunID:        "run-1",
		TurnID:       "turn-1",
		TurnPosition: position,
		Query:        "hello",
	}, runreq.ResolvedContext{}, outcome.RunFailure{Code: apperror.CodeHookUserMessageFailed, Phase: outcome.PhaseSetup}, messagepkg.HistoryFailureOriginUserMessageHook)
	if err != nil {
		t.Fatalf("FailureRow: %v", err)
	}
	if len(persisted) != 2 || len(messages.Persisted) != 2 {
		t.Fatalf("persisted %d rows (%d inputs), want the user row and the assistant row", len(persisted), len(messages.Persisted))
	}
	user, assistant := messages.Persisted[0], messages.Persisted[1]
	if user.Role != "user" || user.TurnID != "turn-1" || user.TurnPosition == nil || *user.TurnPosition != *position {
		t.Fatalf("user row = (%q, %q, %v), want the admitted turn at position %d", user.Role, user.TurnID, user.TurnPosition, *position)
	}
	if assistant.Role != "assistant" || assistant.Metadata[messagepkg.HistoryErrorCodeMetadataKey] != string(apperror.CodeHookUserMessageFailed) {
		t.Fatalf("assistant row = %q %#v, want the hook failure code", assistant.Role, assistant.Metadata)
	}
	for i, input := range messages.Persisted {
		if input.Metadata[messagepkg.HistoryFailureOriginMetadataKey] != messagepkg.HistoryFailureOriginUserMessageHook {
			t.Fatalf("row %d metadata = %#v, want the hook failure origin", i, input.Metadata)
		}
	}
	if len(messages.RoundOptions) != 1 {
		t.Fatalf("round writes = %d, want one atomic write", len(messages.RoundOptions))
	}
}
