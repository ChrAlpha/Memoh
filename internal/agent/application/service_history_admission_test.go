package application

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

func TestAssembleContextDecodesUndecodedAdmissionMetadata(t *testing.T) {
	t.Parallel()

	lifecycle := map[string]any{"version": 2, "selection_decisions": []any{map[string]any{"id": "history:1", "decision": "kept"}}}
	failed := admissionHistoryMessage(t, "failed", sdk.AssistantMessage(""), map[string]any{
		messagepkg.HistoryErrorCodeMetadataKey:  "agent.response_timeout",
		contextfrag.MetadataContextLifecycleKey: lifecycle,
	})
	checkpoint := admissionHistoryMessage(t, "checkpoint", sdk.Message{
		Role:    sdk.MessageRoleAssistant,
		Content: []sdk.MessagePart{sdk.ReasoningPart{Text: "partial reasoning"}, sdk.TextPart{Text: "partial text"}},
	}, map[string]any{
		messagepkg.AgentStepInterruptedMetadataKey: true,
		contextfrag.MetadataContextLifecycleKey:    lifecycle,
	})
	svc := &Service{
		messageService: &memoryQueryMessageService{messages: []messagepkg.Message{
			persistedHistoryMessage(t, "question", "user", "question"), failed, checkpoint,
		}},
		logger: slog.New(slog.DiscardHandler),
	}

	prepared, err := svc.assembleContext(context.Background(), chatContextRequest(contextPurposeRun, ChatRequest{BotID: "bot-1", ThreadID: "session-1"}, 100000))
	if err != nil {
		t.Fatalf("assembleContext failed: %v", err)
	}
	if len(prepared.Messages) != 2 || len(prepared.Records) != 2 {
		t.Fatalf("assembled %d messages, %d records; want the timeout marker dropped", len(prepared.Messages), len(prepared.Records))
	}
	if got := prepared.Messages[1].TextContent(); !strings.Contains(got, messagepkg.AgentStepInterruptedReasoningPrefix+"partial reasoning") {
		t.Fatalf("interrupted checkpoint was not projected: %q", got)
	}
	record := prepared.Records[1]
	if _, ok := record.Metadata[contextfrag.MetadataContextLifecycleKey]; ok {
		t.Fatal("history record kept the lifecycle audit")
	}
	decoded := checkpoint
	if err := json.Unmarshal(checkpoint.RawMetadata, &decoded.Metadata); err != nil {
		t.Fatal(err)
	}
	if want := historyfrag.DBMessageSourceHash(decoded).Value; record.Ref.ContentHash != want {
		t.Fatalf("undecoded row hash = %s, want the decoded row's %s", record.Ref.ContentHash, want)
	}
}

// admissionHistoryMessage mirrors the byte-budgeted loaders, which hand over
// metadata undecoded.
func admissionHistoryMessage(t *testing.T, id string, message sdk.Message, metadata map[string]any) messagepkg.Message {
	t.Helper()
	content, err := json.Marshal(sdkMessagesToModelMessages([]sdk.Message{message})[0])
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	return messagepkg.Message{ID: id, Role: string(message.Role), Content: content, RawMetadata: raw}
}
