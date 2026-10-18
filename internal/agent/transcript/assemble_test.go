package transcript

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/partmeta"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/toolexec"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

func newAssembleService(messages *agenttest.AssembleMessages) *Reader {
	return New(Deps{Messages: messages, Logger: slog.New(slog.DiscardHandler)})
}

func assembleRequest(purpose Purpose, chat runreq.ChatRequest) Request {
	chat.BotID = pipelineTestBotID
	chat.ThreadID = pipelineTestSessionID
	return ForChat(purpose, chat, 100000)
}

func preflightFailure() map[string]any {
	return map[string]any{messagepkg.HistoryFailureOriginMetadataKey: messagepkg.HistoryFailureOriginUserMessageHook}
}

// The rows after the compaction boundary are all there is: nothing is dropped
// by age, and an input rejected before the agent saw it never reaches the
// model, whichever purpose reads the history.
func TestAssembleContextSourcesAndPreflightFailures(t *testing.T) {
	t.Parallel()

	old := agenttest.AssembleBase.Add(-25 * time.Hour)
	rows := []messagepkg.Message{
		agenttest.AssembleText(t, "old-question", "user", "question from yesterday", old),
		agenttest.AssembleText(t, "old-answer", "assistant", "answer from yesterday", old.Add(time.Minute)),
		agenttest.AssembleRow(t, "rejected", sdk.UserMessage("rejected input"), agenttest.AssembleBase.Add(-time.Hour), preflightFailure()),
		agenttest.AssembleRow(t, "rejected-reply", sdk.AssistantMessage("rejected reply"), agenttest.AssembleBase.Add(-time.Hour+time.Second), preflightFailure()),
		agenttest.AssembleText(t, "question", "user", "latest question", agenttest.AssembleBase),
	}
	var users []messagepkg.Message
	for _, row := range rows {
		if row.Role == "user" {
			users = append(users, row)
		}
	}
	cases := []struct {
		name    string
		purpose Purpose
		want    []string
		loader  func(*agenttest.AssembleMessages) (loadedAfter, loadedRecent int)
	}{
		{
			name: "run", purpose: PurposeRun,
			want: []string{"question from yesterday", "answer from yesterday", "latest question"},
		},
		{
			name: "continuation", purpose: PurposeContinuation,
			want: []string{"question from yesterday", "answer from yesterday", "latest question"},
		},
		{
			name: "memory query", purpose: PurposeMemoryQuery,
			want: []string{"question from yesterday", "latest question"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			source := &agenttest.AssembleMessages{After: rows, Recent: users}
			assembled, err := newAssembleService(source).Assemble(context.Background(), assembleRequest(tc.purpose, runreq.ChatRequest{}))
			if err != nil {
				t.Fatalf("assembleContext: %v", err)
			}
			records := make([]runreq.ModelMessage, 0, len(assembled.Records))
			for _, record := range assembled.Records {
				records = append(records, record.ModelMessage)
			}
			agenttest.AssertTexts(t, records, tc.want...)
			if tc.purpose == PurposeMemoryQuery {
				if source.AfterCalls != 0 || source.RecentCalls != 1 {
					t.Fatalf("memory query loaded %d full histories and %d recent reads, want only the recent read", source.AfterCalls, source.RecentCalls)
				}
				return
			}
			agenttest.AssertTexts(t, assembled.Messages, tc.want...)
			if source.AfterCalls != 1 || source.RecentCalls != 0 {
				t.Fatalf("loaded %d full histories and %d recent reads, want one full history", source.AfterCalls, source.RecentCalls)
			}
		})
	}
}

// A request without a session has no stored history to read.
func TestAssembleContextWithoutSessionHasNoHistory(t *testing.T) {
	t.Parallel()

	source := &agenttest.AssembleMessages{After: []messagepkg.Message{agenttest.AssembleText(t, "u", "user", "hello", agenttest.AssembleBase)}}
	request := assembleRequest(PurposeRun, runreq.ChatRequest{})
	request.Scope.SessionID = ""
	assembled, err := newAssembleService(source).Assemble(context.Background(), request)
	if err != nil {
		t.Fatalf("assembleContext: %v", err)
	}
	if len(assembled.Messages) != 0 || source.AfterCalls != 0 {
		t.Fatalf("assembled %d messages after %d loads, want none", len(assembled.Messages), source.AfterCalls)
	}
}

// A summary stands where the rows it replaced started: before everything
// when it predates the surviving rows, between them otherwise.
func TestAssembleContextPlacesSummaryAtItsAnchor(t *testing.T) {
	t.Parallel()

	rows := []messagepkg.Message{
		agenttest.AssembleText(t, "first", "user", "first", agenttest.AssembleBase.Add(time.Minute)),
		agenttest.AssembleText(t, "second", "assistant", "second", agenttest.AssembleBase.Add(3*time.Minute)),
		agenttest.AssembleText(t, "third", "user", "third", agenttest.AssembleBase.Add(4*time.Minute)),
	}
	cases := []struct {
		name   string
		anchor time.Time
		want   []string
	}{
		{"before every surviving row", agenttest.AssembleBase, []string{"<summary>\nearlier window\n</summary>", "first", "second", "third"}},
		{"between surviving rows", agenttest.AssembleBase.Add(2 * time.Minute), []string{"first", "<summary>\nearlier window\n</summary>", "second", "third"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			service := newAssembleService(&agenttest.AssembleMessages{After: rows})
			service.queries = fakeArtifactLineageQueries{rows: []sqlc.BotHistoryMessageCompact{
				compactionLogRow(t, "earlier window", "covered-message", tc.anchor.UnixMilli()),
			}}
			assembled, err := service.Assemble(context.Background(), assembleRequest(PurposeRun, runreq.ChatRequest{}))
			if err != nil {
				t.Fatalf("assembleContext: %v", err)
			}
			agenttest.AssertTexts(t, assembled.Messages, tc.want...)
			if len(ContextFragsForMessages(assembled.Messages, assembled.Records)) != 1 {
				t.Fatalf("summary lost its coverage fragment: %#v", assembled.Records)
			}
			if assembled.CompactableTokens >= estimateMessagesTokens(assembled.Records) {
				t.Fatalf("compactable tokens %d include the summary", assembled.CompactableTokens)
			}
		})
	}
}

// A retry or edit sees the history up to its own turn, and the request row it
// replays is always present.
func TestAssembleContextRetryCutoffAndRequiredRow(t *testing.T) {
	t.Parallel()

	rows := []messagepkg.Message{
		agenttest.AssembleText(t, "u1", "user", "earlier question", agenttest.AssembleBase),
		agenttest.AssembleText(t, "a1", "assistant", "earlier answer", agenttest.AssembleBase.Add(time.Minute)),
		agenttest.AssembleText(t, "u2", "user", "retried question", agenttest.AssembleBase.Add(2*time.Minute)),
		agenttest.AssembleText(t, "a2", "assistant", "replaced answer", agenttest.AssembleBase.Add(3*time.Minute)),
	}
	t.Run("cut at the replaced turn", func(t *testing.T) {
		t.Parallel()

		assembled, err := newAssembleService(&agenttest.AssembleMessages{After: rows}).Assemble(context.Background(),
			assembleRequest(PurposeRun, runreq.ChatRequest{HistoryCutoffBeforeMessageID: "u2"}))
		if err != nil {
			t.Fatalf("assembleContext: %v", err)
		}
		agenttest.AssertTexts(t, assembled.Messages, "earlier question", "earlier answer")
	})
	t.Run("replayed turn already covered by a summary", func(t *testing.T) {
		t.Parallel()

		// The surviving rows all come after the covered turn, so a cut at it
		// leaves nothing before it but the request row itself.
		source := &agenttest.AssembleMessages{After: rows[3:4], Visible: rows[2:4]}
		assembled, err := newAssembleService(source).Assemble(context.Background(),
			assembleRequest(PurposeRun, runreq.ChatRequest{HistoryCutoffBeforeMessageID: "a2", RequiredHistoryMessageID: "u2"}))
		if err != nil {
			t.Fatalf("assembleContext: %v", err)
		}
		agenttest.AssertTexts(t, assembled.Messages, "retried question")
		if last := assembled.Records[len(assembled.Records)-1]; !last.Required || last.DBMessageID != "u2" {
			t.Fatalf("last record = %#v, want the required request row", last)
		}
	})
}

// A request that already stored its own row must not meet that row again in
// the history: by row id when it has one, by the external message id otherwise.
func TestAssembleContextDedupesThePersistedCurrentRow(t *testing.T) {
	t.Parallel()

	current := agenttest.AssembleText(t, "row-current", "user", "continue the task", agenttest.AssembleBase.Add(time.Minute))
	current.ExternalMessageID = "external-current"
	rows := []messagepkg.Message{
		agenttest.AssembleText(t, "row-earlier", "user", "earlier", agenttest.AssembleBase),
		current,
	}
	cases := []struct {
		name string
		chat runreq.ChatRequest
		want []string
	}{
		{"by row id without an external id", runreq.ChatRequest{UserMessagePersisted: true, PersistedUserMessageID: "row-current"}, []string{"earlier"}},
		{"by external id without a row id", runreq.ChatRequest{UserMessagePersisted: true, ExternalMessageID: "external-current"}, []string{"earlier"}},
		{"row id the history does not hold", runreq.ChatRequest{UserMessagePersisted: true, PersistedUserMessageID: "row-elsewhere"}, []string{"earlier", "continue the task"}},
		{"request that persisted nothing", runreq.ChatRequest{PersistedUserMessageID: "row-current"}, []string{"earlier", "continue the task"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assembled, err := newAssembleService(&agenttest.AssembleMessages{After: rows}).Assemble(context.Background(), assembleRequest(PurposeRun, tc.chat))
			if err != nil {
				t.Fatalf("assembleContext: %v", err)
			}
			agenttest.AssertTexts(t, assembled.Messages, tc.want...)
		})
	}
}

// Old tool exchanges and reasoning stay in the history exactly as stored; the
// provider decides what to replay of them.
func TestAssembleContextKeepsToolExchangesAndReasoning(t *testing.T) {
	t.Parallel()

	reasoning := sdk.ReasoningPart{Text: "old thinking", ProviderMetadata: partmeta.Fold(map[string]any{"anthropic": map[string]any{"signature": "SIG_OLD"}})}
	var rows []messagepkg.Message
	at := agenttest.AssembleBase
	add := func(id string, message sdk.Message) {
		rows = append(rows, agenttest.AssembleRow(t, id, message, at, nil))
		at = at.Add(time.Minute)
	}
	add("u0", sdk.UserMessage("read the file"))
	add("a0", sdk.Message{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{
		reasoning,
		sdk.TextPart{Text: "reading"},
		sdk.ToolCallPart{ToolCallID: "call-0", ToolName: "read_file", Input: toolexec.ArgumentsFromValue(map[string]any{"path": "a.txt"})},
	}})
	add("t0", sdk.ToolMessage(sdk.ToolResultPart{ToolCallID: "call-0", ToolName: "read_file", Result: toolexec.OutputFromValue("file body")}))
	add("a0-final", sdk.AssistantMessage("done"))
	for i := range 4 {
		add("u"+string(rune('1'+i)), sdk.UserMessage("question"))
		add("a"+string(rune('1'+i)), sdk.AssistantMessage("answer"))
	}
	if len(rows) <= 10 {
		t.Fatalf("fixture has %d rows, want more than the old stripping threshold", len(rows))
	}

	assembled, err := newAssembleService(&agenttest.AssembleMessages{After: rows}).Assemble(context.Background(), assembleRequest(PurposeRun, runreq.ChatRequest{}))
	if err != nil {
		t.Fatalf("assembleContext: %v", err)
	}
	messages := NormalizeContextMessages(assembled.Messages)
	if len(messages) != len(rows) {
		t.Fatalf("messages = %d, want all %d stored rows", len(messages), len(rows))
	}
	for i, row := range rows {
		var stored runreq.ModelMessage
		if err := json.Unmarshal(row.Content, &stored); err != nil {
			t.Fatal(err)
		}
		got, _ := json.Marshal(runreq.ModelMessageToSDKMessage(messages[i]))
		want, _ := json.Marshal(runreq.ModelMessageToSDKMessage(stored))
		if string(got) != string(want) {
			t.Fatalf("message %d changed:\ngot  %s\nwant %s", i, got, want)
		}
	}
	var sawReasoning, sawResult bool
	for _, part := range runreq.ModelMessageToSDKMessage(messages[1]).Content {
		if _, ok := part.(sdk.ReasoningPart); ok {
			sawReasoning = true
		}
	}
	for _, part := range runreq.ModelMessageToSDKMessage(messages[2]).Content {
		if _, ok := part.(sdk.ToolResultPart); ok {
			sawResult = true
		}
	}
	if !sawReasoning || !sawResult {
		t.Fatalf("old turn lost its reasoning (%t) or its tool result (%t)", sawReasoning, sawResult)
	}
}

// Provider metadata on a text or tool-call part (Gemini thoughtSignature)
// reaches the request unchanged.
func TestAssembleContextKeepsPartProviderMetadata(t *testing.T) {
	t.Parallel()

	toolMeta := partmeta.Fold(map[string]any{"google": map[string]any{"thoughtSignature": "sig-1"}})
	textMeta := partmeta.Fold(map[string]any{"google": map[string]any{"thoughtSignature": "SIG_TEXT"}})
	rows := []messagepkg.Message{
		agenttest.AssembleRow(t, "a-tool", sdk.Message{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{
			sdk.ToolCallPart{ToolCallID: "call-1", ToolName: "read", Input: toolexec.ArgumentsFromValue(map[string]any{"path": "/tmp/a.txt"}), ProviderMetadata: toolMeta},
		}}, agenttest.AssembleBase, nil),
		agenttest.AssembleRow(t, "a-text", sdk.Message{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{
			sdk.TextPart{Text: "the answer", ProviderMetadata: textMeta},
		}}, agenttest.AssembleBase.Add(time.Minute), nil),
	}

	assembled, err := newAssembleService(&agenttest.AssembleMessages{After: rows}).Assemble(context.Background(), assembleRequest(PurposeRun, runreq.ChatRequest{}))
	if err != nil {
		t.Fatalf("assembleContext: %v", err)
	}
	if len(assembled.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(assembled.Messages))
	}
	call, ok := runreq.ModelMessageToSDKMessage(assembled.Messages[0]).Content[0].(sdk.ToolCallPart)
	if !ok || call.ProviderMetadata["google"]["thoughtSignature"] != "sig-1" {
		t.Fatalf("tool-call part = %#v, want thoughtSignature sig-1", call)
	}
	text, ok := runreq.ModelMessageToSDKMessage(assembled.Messages[1]).Content[0].(sdk.TextPart)
	if !ok || text.ProviderMetadata["google"]["thoughtSignature"] != "SIG_TEXT" {
		t.Fatalf("text part = %#v, want thoughtSignature SIG_TEXT", text)
	}
}

// A load that reaches its byte budget may have stopped short of the oldest
// row; the assembly says so, and a load that fits does not.
func TestAssembleContextFlagsALoadThatReachedItsBudget(t *testing.T) {
	t.Parallel()

	rows := []messagepkg.Message{
		agenttest.AssembleText(t, "u1", "user", strings.Repeat("x", 4000), agenttest.AssembleBase),
		agenttest.AssembleText(t, "u2", "user", strings.Repeat("y", 4000), agenttest.AssembleBase.Add(time.Minute)),
	}
	cases := []struct {
		name   string
		budget int
		want   bool
	}{
		{"budget below the rows", 1000, true},
		{"budget above the rows", 100000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			source := &agenttest.AssembleMessages{After: rows}
			request := assembleRequest(PurposeRun, runreq.ChatRequest{})
			request.Budget = tc.budget
			assembled, err := newAssembleService(source).Assemble(context.Background(), request)
			if err != nil {
				t.Fatalf("assembleContext: %v", err)
			}
			if assembled.Truncated != tc.want {
				t.Fatalf("Truncated = %t, want %t", assembled.Truncated, tc.want)
			}
			if want := int64(tc.budget) * contextfrag.EstimateBytesPerToken; source.MaxBytes != want {
				t.Fatalf("load budget = %d bytes, want %d", source.MaxBytes, want)
			}
		})
	}
}

// Normalizing keeps every tool exchange and closes a call left open.
func TestNormalizeContextMessagesKeepsToolExchanges(t *testing.T) {
	t.Parallel()

	var messages []runreq.ModelMessage
	messages = append(messages, runreq.ModelMessage{Content: runreq.NewTextContent("missing role")})
	for i := range 6 {
		call := "call-" + string(rune('a'+i))
		messages = append(messages, runreq.SDKMessagesToModelMessages([]sdk.Message{
			sdk.UserMessage("question"),
			{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{sdk.ToolCallPart{ToolCallID: call, ToolName: "web_fetch"}}},
			sdk.ToolMessage(sdk.ToolResultPart{ToolCallID: call, ToolName: "web_fetch", Result: toolexec.OutputFromValue("body")}),
		})...)
	}
	messages = append(messages, runreq.SDKMessagesToModelMessages([]sdk.Message{
		{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{sdk.ToolCallPart{ToolCallID: "open-call", ToolName: "ask_user"}}},
	})...)

	got := NormalizeContextMessages(messages)
	var results int
	for _, message := range got {
		if message.Role == "tool" {
			results++
		}
	}
	if results != 7 {
		t.Fatalf("tool results = %d, want the six exchanges and the closure of the open call", results)
	}
	if got[0].TextContent() != "question" {
		t.Fatalf("first message = %q, want the role-less message dropped", got[0].TextContent())
	}
}
