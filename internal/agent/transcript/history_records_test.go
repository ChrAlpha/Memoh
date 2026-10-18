package transcript

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/context/compaction"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

func TestDedupePersistedCurrentUserMessageUsesHistoryRecordProvenance(t *testing.T) {
	t.Parallel()

	history := []historyfrag.HistoryRecord{
		agenttest.HistoryRecord("row-1", runreq.ModelMessage{
			Role:    "user",
			Content: runreq.NewTextContent("---\nmessage-id: qq-msg-1\nchannel: qq\n---\nhello"),
		}, func(record *historyfrag.HistoryRecord) {
			record.ExternalMessageID = "qq-msg-1"
			record.Platform = "qq"
			record.SenderChannelIdentityID = "channel-identity-1"
		}),
		agenttest.HistoryRecord("row-2", runreq.ModelMessage{
			Role:    "assistant",
			Content: runreq.NewTextContent("ok"),
		}, nil),
	}

	got := dedupePersistedCurrentUserMessage(history, runreq.ChatRequest{
		UserMessagePersisted:    true,
		RouteID:                 "route-1",
		ExternalMessageID:       "qq-msg-1",
		CurrentChannel:          "qq",
		SourceChannelIdentityID: "channel-identity-1",
	})

	if len(got) != 1 {
		t.Fatalf("expected 1 message after dedupe, got %d", len(got))
	}
	if got[0].ModelMessage.Role != "assistant" {
		t.Fatalf("unexpected remaining role: %s", got[0].ModelMessage.Role)
	}
}

func TestReplaceCompactedHistoryRecordsUsesTypedSummaryRecord(t *testing.T) {
	t.Parallel()

	records := []historyfrag.HistoryRecord{
		agenttest.HistoryRecord("row-1", runreq.ModelMessage{Role: "user", Content: runreq.NewTextContent("old 1")}, func(record *historyfrag.HistoryRecord) {
			record.CompactID = "compact-1"
		}),
		agenttest.HistoryRecord("row-2", runreq.ModelMessage{Role: "assistant", Content: runreq.NewTextContent("old 2")}, func(record *historyfrag.HistoryRecord) {
			record.CompactID = "compact-1"
		}),
		agenttest.HistoryRecord("row-3", runreq.ModelMessage{Role: "user", Content: runreq.NewTextContent("new")}, nil),
	}

	got := replaceCompactedHistoryRecords(records, map[string]string{"compact-1": "condensed"}, contextfrag.Scope{})
	wantMessages := []runreq.ModelMessage{
		{Role: "user", Content: runreq.NewTextContent("<summary>\ncondensed\n</summary>")},
		{Role: "user", Content: runreq.NewTextContent("new")},
	}
	if gotMessages := historyfrag.ToModelMessages(got); !reflect.DeepEqual(gotMessages, wantMessages) {
		t.Fatalf("replacement messages mismatch:\ngot  %#v\nwant %#v", gotMessages, wantMessages)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 records, got %d", len(got))
	}
	summary := got[0]
	if summary.SourceKind != historyfrag.SourceCompactionLog || summary.Lifecycle != historyfrag.LifecycleActiveSummary {
		t.Fatalf("summary record source/lifecycle mismatch: %#v", summary)
	}
	if summary.Kind != contextfrag.KindConversationSummary {
		t.Fatalf("summary should be conversation_summary, got %s", summary.Kind)
	}
	if summary.Ref.Namespace != "compaction_log" || summary.Ref.ID != "compact-1" || summary.Ref.Durability != contextfrag.RefDurable {
		t.Fatalf("summary ref should be durable compaction log identity: %#v", summary.Ref)
	}
	if summary.Coverage == nil || len(summary.Coverage.CoveredRefs) != 2 {
		t.Fatalf("summary should cover compacted records: %#v", summary.Coverage)
	}
	if summary.Coverage.CoveredRefs[0].ID != "row-1" || summary.Coverage.CoveredRefs[1].ID != "row-2" {
		t.Fatalf("summary coverage should preserve covered record refs: %#v", summary.Coverage.CoveredRefs)
	}
	if frag := historyfrag.ToFrag(summary); frag.Kind != contextfrag.KindConversationSummary || frag.Slot != contextfrag.SlotHistory || frag.Coverage == nil {
		t.Fatalf("summary frag should carry active summary coverage: %#v", frag)
	}
}

func TestReplaceCompactedHistoryRecordsScopesSummaryToConversationNotFirstSender(t *testing.T) {
	t.Parallel()

	records := []historyfrag.HistoryRecord{
		agenttest.HistoryRecord("row-1", runreq.ModelMessage{Role: "user", Content: runreq.NewTextContent("old 1")}, func(record *historyfrag.HistoryRecord) {
			record.CompactID = "compact-1"
			record.Scope = contextfrag.Scope{
				BotID:             "bot-1",
				ChatID:            "chat-1",
				SessionID:         "sess-1",
				ChannelIdentityID: "sender-1",
				DisplayName:       "Alice",
				CurrentMessageID:  "msg-1",
				EventID:           "evt-1",
				ReplyToMessageID:  "msg-0",
			}
		}),
		agenttest.HistoryRecord("row-2", runreq.ModelMessage{Role: "assistant", Content: runreq.NewTextContent("old 2")}, func(record *historyfrag.HistoryRecord) {
			record.CompactID = "compact-1"
		}),
	}

	conversationScope := SummaryScope("bot-1", "chat-1", "sess-1", "group", "Dev Chat", "target-1")
	got := replaceCompactedHistoryRecords(records, map[string]string{"compact-1": "condensed"}, conversationScope)
	if len(got) != 1 {
		t.Fatalf("expected 1 record, got %d", len(got))
	}

	scope := got[0].Scope
	if scope.ChannelIdentityID != "" || scope.DisplayName != "" || scope.CurrentMessageID != "" ||
		scope.EventID != "" || scope.ReplyToMessageID != "" {
		t.Fatalf("summary scope must not carry first sender's identity: %#v", scope)
	}
	if scope.BotID != "bot-1" || scope.ChatID != "chat-1" || scope.SessionID != "sess-1" ||
		scope.ConversationType != "group" || scope.ConversationName != "Dev Chat" || scope.ReplyTarget != "target-1" {
		t.Fatalf("summary scope must carry conversation topology: %#v", scope)
	}
}

func TestReplaceCompactedHistoryRecordsKeepsOriginalGroupWithoutSummary(t *testing.T) {
	t.Parallel()

	records := []historyfrag.HistoryRecord{
		agenttest.HistoryRecord("row-1", runreq.ModelMessage{Role: "user", Content: runreq.NewTextContent("old 1")}, func(record *historyfrag.HistoryRecord) {
			record.CompactID = "compact-1"
		}),
		agenttest.HistoryRecord("row-2", runreq.ModelMessage{Role: "assistant", Content: runreq.NewTextContent("old 2")}, func(record *historyfrag.HistoryRecord) {
			record.CompactID = "compact-1"
		}),
	}

	gotMissing := replaceCompactedHistoryRecords(records, map[string]string{}, contextfrag.Scope{})
	if gotMessages := historyfrag.ToModelMessages(gotMissing); !reflect.DeepEqual(gotMessages, historyfrag.ToModelMessages(records)) {
		t.Fatalf("missing summary should keep original group:\ngot  %#v\nwant %#v", gotMessages, historyfrag.ToModelMessages(records))
	}

	gotEmpty := replaceCompactedHistoryRecords(records, map[string]string{"compact-1": ""}, contextfrag.Scope{})
	if gotMessages := historyfrag.ToModelMessages(gotEmpty); !reflect.DeepEqual(gotMessages, historyfrag.ToModelMessages(records)) {
		t.Fatalf("empty summary should keep original group:\ngot  %#v\nwant %#v", gotMessages, historyfrag.ToModelMessages(records))
	}

	// A legacy status='ok' log can hold a whitespace-only summary (main never
	// trimmed). Substituting it would drop the raw rows for nothing, and the
	// reclaim SQL treats such rows as still eligible — the read path must agree.
	gotWhitespace := replaceCompactedHistoryRecords(records, map[string]string{"compact-1": "  \n\t"}, contextfrag.Scope{})
	if gotMessages := historyfrag.ToModelMessages(gotWhitespace); !reflect.DeepEqual(gotMessages, historyfrag.ToModelMessages(records)) {
		t.Fatalf("whitespace-only summary should keep original group:\ngot  %#v\nwant %#v", gotMessages, historyfrag.ToModelMessages(records))
	}
}

func TestReplaceCompactedHistoryRecordsKeepsMustKeepIslandOrdering(t *testing.T) {
	t.Parallel()

	// The selector now marks only the contiguous run before a must-keep island
	// (the ask_user exchange) under one compact_id; the island and the run after
	// it stay raw. The read path must drop the summary in place and preserve
	// order — "mid q" stays AFTER the ask_user exchange, never folded before it.
	records := []historyfrag.HistoryRecord{
		agenttest.HistoryRecord("row-old", runreq.ModelMessage{Role: "user", Content: runreq.NewTextContent("old q")}, func(r *historyfrag.HistoryRecord) {
			r.CompactID = "compact-1"
		}),
		agenttest.HistoryRecord("row-ask-call", runreq.ModelMessage{Role: "assistant", Content: runreq.NewTextContent("ask you something")}, nil),
		agenttest.HistoryRecord("row-ask-result", runreq.ModelMessage{Role: "tool", Content: runreq.NewTextContent("answered")}, nil),
		agenttest.HistoryRecord("row-mid", runreq.ModelMessage{Role: "user", Content: runreq.NewTextContent("mid q")}, nil),
		agenttest.HistoryRecord("row-current", runreq.ModelMessage{Role: "user", Content: runreq.NewTextContent("current")}, nil),
	}

	got := replaceCompactedHistoryRecords(records, map[string]string{"compact-1": "condensed"}, contextfrag.Scope{})
	want := []runreq.ModelMessage{
		{Role: "user", Content: runreq.NewTextContent("<summary>\ncondensed\n</summary>")},
		{Role: "assistant", Content: runreq.NewTextContent("ask you something")},
		{Role: "tool", Content: runreq.NewTextContent("answered")},
		{Role: "user", Content: runreq.NewTextContent("mid q")},
		{Role: "user", Content: runreq.NewTextContent("current")},
	}
	if gotMessages := historyfrag.ToModelMessages(got); !reflect.DeepEqual(gotMessages, want) {
		t.Fatalf("must-keep island ordering broken:\ngot  %#v\nwant %#v", gotMessages, want)
	}
}

func TestTotalCompactableHistoryTokensExcludesSummaries(t *testing.T) {
	t.Parallel()

	summary := historyfrag.SummaryRecord("compact-big", strings.Repeat("s", 4000), nil, contextfrag.Scope{})
	raw := agenttest.HistoryRecord("row-1", runreq.ModelMessage{Role: "user", Content: runreq.NewTextContent(strings.Repeat("r", 400))}, nil)
	records := []historyfrag.HistoryRecord{summary, raw}

	compactable := totalCompactableHistoryTokens(records)
	if compactable <= 0 {
		t.Fatal("raw rows must count toward the compactable estimate")
	}
	if want := EstimateMessageTokens(raw.ModelMessage); compactable != want {
		t.Fatalf("compactable = %d, want raw-only estimate %d", compactable, want)
	}
}

func TestHistoryScopeFallbackFromChatRequestUsesRequestTopology(t *testing.T) {
	t.Parallel()

	got := scopeFallbackFromChatRequest(runreq.ChatRequest{
		ChatID:           " chat-1 ",
		ConversationType: " group ",
		ConversationName: " Dev Chat ",
		ReplyTarget:      " target-1 ",
	})

	if got.ChatID != "chat-1" ||
		got.ConversationType != "group" ||
		got.ConversationName != "Dev Chat" ||
		got.ReplyTarget != "target-1" {
		t.Fatalf("unexpected fallback: %#v", got)
	}
}

func dbHistoryRow(t *testing.T, id string, role string, content json.RawMessage, mutate func(*messagepkg.Message)) messagepkg.Message {
	t.Helper()
	msg := messagepkg.Message{
		ID:      id,
		BotID:   "bot-1",
		Role:    role,
		Content: content,
	}
	if mutate != nil {
		mutate(&msg)
	}
	return msg
}

func mustRawJSON(t *testing.T, value any) json.RawMessage {
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

func persistedCoverage(t *testing.T, id string) []byte {
	t.Helper()
	raw, err := json.Marshal([]compaction.CoveredSource{{
		Ref: contextfrag.ContextRef{
			Namespace:   "bot_history_message",
			ID:          id,
			Version:     1,
			HashAlgo:    contextfrag.HashAlgoSHA256,
			HashScope:   contextfrag.HashScopeSourcePayload,
			ContentHash: agenttest.HistorySourceHash(id),
			Schema:      contextfrag.SchemaContextRef,
			Durability:  contextfrag.RefDurable,
		},
	}})
	if err != nil {
		t.Fatalf("marshal persisted coverage: %v", err)
	}
	return raw
}

func recordTexts(records []historyfrag.HistoryRecord) []string {
	texts := make([]string, 0, len(records))
	for _, record := range records {
		texts = append(texts, record.ModelMessage.TextContent())
	}
	return texts
}

func mustReplaceCompactedMessages(t *testing.T, resolver *Reader, sessionID string, scope contextfrag.Scope, records []historyfrag.HistoryRecord) []historyfrag.HistoryRecord {
	t.Helper()
	replaced, err := resolver.replaceCompactedMessages(context.Background(), sessionID, scope, records, ArtifactBoundary{})
	if err != nil {
		t.Fatalf("replaceCompactedMessages: %v", err)
	}
	return replaced
}
