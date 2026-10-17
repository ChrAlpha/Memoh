package application

import (
	"reflect"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/toolexec"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

func TestHistoryRecordPathPreservesLegacyServiceMessagePipeline(t *testing.T) {
	t.Parallel()

	assistantToolCallSDK := sdk.Message{
		Role: sdk.MessageRoleAssistant,
		Content: []sdk.MessagePart{
			sdk.ToolCallPart{
				ToolCallID: "call-1",
				ToolName:   "lookup",
				Input:      toolexec.ArgumentsFromValue(map[string]any{"q": "memoh"}),
			},
		},
	}
	assistantToolCall := runreq.SDKMessagesToModelMessages([]sdk.Message{assistantToolCallSDK})[0]
	toolResultSDK := sdk.ToolMessage(sdk.ToolResultPart{
		ToolCallID: "call-1",
		ToolName:   "lookup",
		Result:     toolexec.OutputFromValue("tool result"),
	})
	toolResult := runreq.SDKMessagesToModelMessages([]sdk.Message{toolResultSDK})[0]
	rows := []messagepkg.Message{
		dbHistoryRow(t, "row-compact-user", "user", runreq.NewTextContent("old compacted user"), func(msg *messagepkg.Message) {
			msg.CompactID = "compact-ok"
		}),
		dbHistoryRow(t, "row-compact-assistant", "assistant", runreq.NewTextContent("old compacted assistant"), func(msg *messagepkg.Message) {
			msg.CompactID = "compact-ok"
		}),
		dbHistoryRow(t, "row-missing-summary", "user", runreq.NewTextContent("missing summary body"), func(msg *messagepkg.Message) {
			msg.CompactID = "compact-missing"
		}),
		dbHistoryRow(t, "row-current", "user", runreq.NewTextContent("already persisted current"), func(msg *messagepkg.Message) {
			msg.SessionID = "sess-1"
			msg.ExternalMessageID = "msg-current"
			msg.Platform = "telegram"
			msg.SenderChannelIdentityID = "sender-1"
		}),
		{
			ID:      "row-plain",
			BotID:   "bot-1",
			Role:    "user",
			Content: runreq.NewTextContent("plain string content"),
		},
		dbHistoryRow(t, "row-tool-call", "assistant", mustRawJSON(t, assistantToolCall), nil),
		dbHistoryRow(t, "row-tool-result", "tool", mustRawJSON(t, toolResult), nil),
	}

	records := make([]historyfrag.HistoryRecord, 0, len(rows))
	for _, row := range rows {
		record, err := historyfrag.FromDBMessage(row, historyfrag.ScopeFallback{ChatID: "chat-1"})
		if err != nil {
			t.Fatalf("FromDBMessage(%s): %v", row.ID, err)
		}
		records = append(records, record)
	}
	records = dedupePersistedCurrentUserMessage(records, runreq.ChatRequest{
		UserMessagePersisted:    true,
		ThreadID:                "sess-1",
		ExternalMessageID:       "msg-current",
		CurrentChannel:          "telegram",
		SourceChannelIdentityID: "sender-1",
	})
	records = replaceCompactedHistoryRecords(records, map[string]string{"compact-ok": "condensed"}, contextfrag.Scope{})
	annotated := injectWorkspaceTransitionRecords(records)
	got := make([]runreq.ModelMessage, 0, len(annotated))
	for _, record := range annotated {
		got = append(got, record.ModelMessage)
	}
	tokens := estimateMessagesTokens(annotated)

	want := []runreq.ModelMessage{
		{Role: "user", Content: runreq.NewTextContent("<summary>\ncondensed\n</summary>")},
		{Role: "user", Content: runreq.NewTextContent("missing summary body")},
		{Role: "user", Content: runreq.NewTextContent("plain string content")},
		assistantToolCall,
		toolResult,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("history pipeline payload mismatch:\ngot  %#v\nwant %#v", got, want)
	}
	if tokens == 0 {
		t.Fatal("history pipeline should report estimated tokens for retained records")
	}

	repaired := runreq.RepairToolCallClosures(runreq.SanitizeMessages(got), runreq.SyntheticToolClosureError)
	assertSameJSON(t, runreq.ModelMessagesToSDKMessages(runreq.NonNilModelMessages(repaired)), []sdk.Message{
		sdk.UserMessage("<summary>\ncondensed\n</summary>"),
		sdk.UserMessage("missing summary body"),
		sdk.UserMessage("plain string content"),
		assistantToolCallSDK,
		toolResultSDK,
	})
}
