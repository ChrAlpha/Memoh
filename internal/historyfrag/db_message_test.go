package historyfrag

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/conversation"
	messagepkg "github.com/memohai/memoh/internal/message"
)

func TestFromDBMessageBuildsDurableTurnDAGRecordAndFrag(t *testing.T) {
	t.Parallel()

	inputTokens := 12
	outputTokens := 34
	msg := messagepkg.Message{
		ID:                      "row-1",
		BotID:                   "bot-1",
		SessionID:               "session-1",
		TurnID:                  "turn-1",
		TurnMessageSeq:          7,
		SenderChannelIdentityID: "sender-1",
		SenderUserID:            "user-1",
		SenderDisplayName:       "Alice",
		Platform:                "telegram",
		ExternalMessageID:       "external-1",
		SourceReplyToMessageID:  "external-0",
		Role:                    "user",
		Content:                 persistedModelMessage(t, conversation.ModelMessage{Role: "user", Content: conversation.NewTextContent("hello")}),
		Metadata:                map[string]any{"reply": map[string]any{"sender": "Bob"}},
		Usage:                   mustJSON(t, map[string]int{"inputTokens": inputTokens, "output_tokens": outputTokens}),
		SessionMode:             "chat",
		RuntimeType:             "model",
		Assets: []messagepkg.MessageAsset{{
			ContentHash: "asset-hash-1",
			Role:        "attachment",
			Ordinal:     2,
			Name:        "image.png",
			Metadata:    map[string]any{"width": float64(640)},
		}},
		CompactID:      "compact-1",
		EventID:        "event-1",
		DisplayContent: "hello",
		CreatedAt:      time.Date(2026, 6, 24, 3, 0, 0, 0, time.UTC),
	}

	record, err := FromDBMessage(msg, ScopeFallback{
		ChatID:           "chat-1",
		ViewHeadTurnID:   "head-1",
		ConversationType: "group",
		ConversationName: "Dev Chat",
		ReplyTarget:      "target-1",
	})
	if err != nil {
		t.Fatalf("FromDBMessage failed: %v", err)
	}

	if record.Ref.Namespace != NamespaceDBHistoryMessage || record.Ref.ID != "row-1" || record.Ref.Durability != contextfrag.RefDurable {
		t.Fatalf("unexpected durable ref: %#v", record.Ref)
	}
	if record.Ref.HashScope != contextfrag.HashScopeSourcePayload || record.Ref.ContentHash == "" {
		t.Fatalf("ref should carry source payload hash: %#v", record.Ref)
	}
	if record.TurnID != "turn-1" || record.TurnMessageSeq != 7 {
		t.Fatalf("record lost turn ordering: turn=%q seq=%d", record.TurnID, record.TurnMessageSeq)
	}
	if record.SessionMode != "chat" || record.RuntimeType != "model" {
		t.Fatalf("record lost session/runtime type: mode=%q runtime=%q", record.SessionMode, record.RuntimeType)
	}
	if record.Scope.ViewHeadTurnID != "head-1" {
		t.Fatalf("record lost selected view head: %#v", record.Scope)
	}
	if record.UsageInputTokens == nil || *record.UsageInputTokens != inputTokens {
		t.Fatalf("input tokens = %#v, want %d", record.UsageInputTokens, inputTokens)
	}
	if record.UsageOutputTokens == nil || *record.UsageOutputTokens != outputTokens {
		t.Fatalf("output tokens = %#v, want %d", record.UsageOutputTokens, outputTokens)
	}
	if len(record.Assets) != 1 || record.Assets[0].ContentHash != "asset-hash-1" || record.Assets[0].Metadata["width"] != float64(640) {
		t.Fatalf("record lost media refs: %#v", record.Assets)
	}

	frag := ToFrag(record)
	if err := contextfrag.ValidateContextRef(frag.Ref); err != nil {
		t.Fatalf("frag ref invalid: %#v: %v", frag.Ref, err)
	}
	if frag.Scope.TurnID != "turn-1" || frag.Scope.ViewHeadTurnID != "head-1" || frag.Scope.TurnMessageSeq != 7 {
		t.Fatalf("frag lost Turn DAG scope: %#v", frag.Scope)
	}
	if frag.Scope.SessionMode != "chat" || frag.Scope.RuntimeType != "model" {
		t.Fatalf("frag lost session/runtime scope: %#v", frag.Scope)
	}
	if frag.Trust != contextfrag.TrustExternal {
		t.Fatalf("history frag trust = %s, want %s", frag.Trust, contextfrag.TrustExternal)
	}
	if frag.Provenance.Source != string(SourceDBMessage) || frag.Provenance.SourceID != "row-1" || frag.Provenance.Collector != CollectorHistoryRecords {
		t.Fatalf("unexpected frag provenance: %#v", frag.Provenance)
	}
	if got := ToSDKMessages([]HistoryRecord{record}); len(got) != 1 || got[0].Role != sdk.MessageRoleUser {
		t.Fatalf("ToSDKMessages = %#v", got)
	}
}

func TestDBMessageSourceHashTracksPersistedTurnDAGFields(t *testing.T) {
	t.Parallel()

	base := messagepkg.Message{
		ID:             "row-1",
		BotID:          "bot-1",
		SessionID:      "session-1",
		TurnID:         "turn-1",
		TurnMessageSeq: 1,
		Role:           "user",
		Content:        persistedModelMessage(t, conversation.ModelMessage{Role: "user", Content: conversation.NewTextContent("same")}),
		SessionMode:    "chat",
		RuntimeType:    "model",
	}
	changedTurn := base
	changedTurn.TurnID = "turn-2"
	changedSeq := base
	changedSeq.TurnMessageSeq = 2
	changedRuntime := base
	changedRuntime.RuntimeType = "acp_agent"

	hash := DBMessageSourceHash(base).Value
	if hash == DBMessageSourceHash(changedTurn).Value {
		t.Fatal("turn id change should affect DB source hash")
	}
	if hash == DBMessageSourceHash(changedSeq).Value {
		t.Fatal("turn message seq change should affect DB source hash")
	}
	if hash == DBMessageSourceHash(changedRuntime).Value {
		t.Fatal("runtime type change should affect DB source hash")
	}

	first, err := FromDBMessage(base, ScopeFallback{ViewHeadTurnID: "head-a"})
	if err != nil {
		t.Fatalf("first FromDBMessage failed: %v", err)
	}
	second, err := FromDBMessage(base, ScopeFallback{ViewHeadTurnID: "head-b"})
	if err != nil {
		t.Fatalf("second FromDBMessage failed: %v", err)
	}
	if first.Ref.ContentHash != second.Ref.ContentHash {
		t.Fatalf("selected view head fallback must not change persisted row hash: first=%#v second=%#v", first.Ref, second.Ref)
	}
}

func TestDBMessageSourceHashTracksPersistedPayloadFields(t *testing.T) {
	t.Parallel()

	base := messagepkg.Message{
		ID:        "row-1",
		BotID:     "bot-1",
		SessionID: "session-1",
		Role:      "user",
		Content:   persistedModelMessage(t, conversation.ModelMessage{Role: "user", Content: conversation.NewTextContent("hello")}),
		Metadata:  map[string]any{"reply": map[string]any{"sender": "Alice"}},
		Usage:     mustJSON(t, map[string]int{"inputTokens": 1, "outputTokens": 2}),
		Assets: []messagepkg.MessageAsset{{
			ContentHash: "asset-hash-1",
			Role:        "attachment",
			Ordinal:     0,
			Name:        "image.png",
			Metadata:    map[string]any{"width": float64(640)},
		}},
	}

	hash := DBMessageSourceHash(base).Value
	const want = "703373472a5910ff4f771ac1e05494fefe0f04e123dc3cfb40bdee0a382b3d71"
	if hash != want {
		t.Fatalf("source hash drifted: got %q, want %q", hash, want)
	}

	changedContent := base
	changedContent.Content = persistedModelMessage(t, conversation.ModelMessage{Role: "user", Content: conversation.NewTextContent("hello again")})
	changedMetadata := base
	changedMetadata.Metadata = map[string]any{"reply": map[string]any{"sender": "Bob"}}
	changedUsage := base
	changedUsage.Usage = mustJSON(t, map[string]int{"inputTokens": 1, "outputTokens": 3})
	changedAsset := base
	changedAsset.Assets = append([]messagepkg.MessageAsset(nil), base.Assets...)
	changedAsset.Assets[0].ContentHash = "asset-hash-2"

	for name, msg := range map[string]messagepkg.Message{
		"content":  changedContent,
		"metadata": changedMetadata,
		"usage":    changedUsage,
		"asset":    changedAsset,
	} {
		if DBMessageSourceHash(msg).Value == hash {
			t.Fatalf("%s change should affect DB source hash", name)
		}
	}
}

func TestSummaryRecordCarriesCoverageAndKeepBudget(t *testing.T) {
	t.Parallel()

	covered := []contextfrag.ContextRef{{
		Namespace:  NamespaceDBHistoryMessage,
		ID:         "row-1",
		Schema:     contextfrag.SchemaContextRef,
		Durability: contextfrag.RefDurable,
	}}

	record := SummaryRecord("compact-1", "condensed", covered, contextfrag.Scope{
		SessionID:      "session-1",
		ViewHeadTurnID: "head-1",
	})

	if record.Kind != contextfrag.KindConversationSummary || record.Lifecycle != LifecycleActiveSummary {
		t.Fatalf("summary kind/lifecycle = %s/%s", record.Kind, record.Lifecycle)
	}
	if record.Budget.Overflow != contextfrag.OverflowKeep {
		t.Fatalf("summary budget = %#v, want keep", record.Budget)
	}
	if record.Coverage == nil || len(record.Coverage.CoveredRefs) != 1 || !record.Coverage.CoveredRefs[0].EqualIdentity(covered[0]) {
		t.Fatalf("summary coverage = %#v", record.Coverage)
	}
	if ToFrag(record).Coverage == nil {
		t.Fatal("summary frag should expose coverage")
	}
}

func TestHistoryRecordsRenderLegacyModelAndSDKMessages(t *testing.T) {
	t.Parallel()

	expectedModel := []conversation.ModelMessage{
		{Role: "user", Content: conversation.NewTextContent("hello")},
		{
			Role: "assistant",
			Content: mustJSON(t, []map[string]any{
				{"type": "text", "text": "checking"},
				{"type": "tool-call", "toolCallId": "call-1", "toolName": "lookup", "input": map[string]any{"q": "memoh"}},
			}),
		},
	}
	records := make([]HistoryRecord, 0, len(expectedModel))
	for i, msg := range expectedModel {
		record, err := FromDBMessage(messagepkg.Message{
			ID:      string(rune('a' + i)),
			BotID:   "bot-1",
			Role:    msg.Role,
			Content: persistedModelMessage(t, msg),
		}, ScopeFallback{})
		if err != nil {
			t.Fatalf("FromDBMessage %d failed: %v", i, err)
		}
		records = append(records, record)
	}

	if got := ToModelMessages(records); !reflect.DeepEqual(got, expectedModel) {
		t.Fatalf("ToModelMessages mismatch:\ngot  %#v\nwant %#v", got, expectedModel)
	}

	assertSameJSON(t, ToSDKMessages(records), []sdk.Message{
		sdk.UserMessage("hello"),
		{
			Role: sdk.MessageRoleAssistant,
			Content: []sdk.MessagePart{
				sdk.TextPart{Text: "checking"},
				sdk.ToolCallPart{ToolCallID: "call-1", ToolName: "lookup", Input: map[string]any{"q": "memoh"}},
			},
		},
	})
}

func TestModelMessageToSDKPreservesTopLevelToolCalls(t *testing.T) {
	t.Parallel()

	mm := conversation.ModelMessage{
		Role:    "assistant",
		Content: conversation.NewTextContent("I will look that up."),
		ToolCalls: []conversation.ToolCall{{
			ID:   "call-42",
			Type: "function",
			Function: conversation.ToolCallFunction{
				Name:      "ask_user",
				Arguments: `{"question":"Are you sure?"}`,
			},
		}},
	}

	record := HistoryRecord{
		Ref:          contextfrag.ContextRef{Namespace: "test", ID: "tc-1", Schema: contextfrag.SchemaContextRef},
		Kind:         contextfrag.KindConversationEvent,
		SourceKind:   SourceDBMessage,
		ModelMessage: mm,
	}
	frag := ToFrag(record)

	var foundToolCall bool
	for _, part := range frag.Parts {
		msg := part.SDKMessage
		if msg == nil {
			msg = part.Message
		}
		if msg == nil {
			continue
		}
		for _, cp := range msg.Content {
			if tcp, ok := cp.(sdk.ToolCallPart); ok {
				foundToolCall = true
				if tcp.ToolCallID != "call-42" {
					t.Fatalf("ToolCallID = %q, want call-42", tcp.ToolCallID)
				}
				if tcp.ToolName != "ask_user" {
					t.Fatalf("ToolName = %q, want ask_user", tcp.ToolName)
				}
			}
		}
	}
	if !foundToolCall {
		t.Fatal("frag should contain a ToolCallPart from top-level ToolCalls")
	}
}

func TestModelMessageToSDKPreservesTopLevelToolResultName(t *testing.T) {
	t.Parallel()

	mm := conversation.ModelMessage{
		Role:       "tool",
		Content:    conversation.NewTextContent("42"),
		ToolCallID: "call-42",
		Name:       "calculator",
	}

	msg := ToSDKMessages([]HistoryRecord{{
		Ref:          contextfrag.ContextRef{Namespace: "test", ID: "tr-1", Schema: contextfrag.SchemaContextRef},
		Kind:         contextfrag.KindConversationEvent,
		SourceKind:   SourceDBMessage,
		ModelMessage: mm,
	}})

	if len(msg) != 1 {
		t.Fatalf("got %d messages, want 1", len(msg))
	}
	if msg[0].Role != sdk.MessageRoleTool {
		t.Fatalf("role = %q, want tool", msg[0].Role)
	}
}

func TestModelMessageToSDKDoesNotDuplicateContentToolCalls(t *testing.T) {
	t.Parallel()

	mm := conversation.ModelMessage{
		Role: "assistant",
		Content: mustJSON(t, []map[string]any{
			{"type": "text", "text": "checking"},
			{"type": "tool-call", "toolCallId": "call-1", "toolName": "lookup", "input": map[string]any{"q": "memoh"}},
		}),
		ToolCalls: []conversation.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: conversation.ToolCallFunction{
				Name:      "lookup",
				Arguments: `{"q":"memoh"}`,
			},
		}},
	}

	msg := ToSDKMessages([]HistoryRecord{{
		Ref:          contextfrag.ContextRef{Namespace: "test", ID: "dup-1", Schema: contextfrag.SchemaContextRef},
		Kind:         contextfrag.KindConversationEvent,
		SourceKind:   SourceDBMessage,
		ModelMessage: mm,
	}})

	toolCallCount := 0
	for _, part := range msg[0].Content {
		if _, ok := part.(sdk.ToolCallPart); ok {
			toolCallCount++
		}
	}
	if toolCallCount != 1 {
		t.Fatalf("tool call parts = %d, want 1 (no duplicates)", toolCallCount)
	}
}

func persistedModelMessage(t *testing.T, msg conversation.ModelMessage) json.RawMessage {
	t.Helper()
	return mustJSON(t, msg)
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %T: %v", value, err)
	}
	return raw
}

func assertSameJSON(t *testing.T, got any, want any) {
	t.Helper()
	gotRaw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	wantRaw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	if string(gotRaw) != string(wantRaw) {
		t.Fatalf("json mismatch:\ngot  %s\nwant %s", gotRaw, wantRaw)
	}
}
