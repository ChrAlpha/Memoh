package application

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/partmeta"
	"github.com/felinics/memoh/internal/agent/toolexec"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// assembleMessageService serves a session's context rows the way the byte
// budgeted loaders do and counts which loader a purpose used.
type assembleMessageService struct {
	recordingMessageService
	after   []messagepkg.Message
	recent  []messagepkg.Message
	visible []messagepkg.Message

	afterCalls  int
	recentCalls int
	maxBytes    int64
}

func (s *assembleMessageService) ListActiveAfterCompactionBySessionWithinBytes(_ context.Context, _ string, maxBytes int64) (messagepkg.SessionHistoryAfterCompaction, error) {
	s.afterCalls++
	s.maxBytes = maxBytes
	return messagepkg.SessionHistoryAfterCompaction{Messages: append([]messagepkg.Message(nil), s.after...)}, nil
}

func (s *assembleMessageService) ListRecentUserBySession(context.Context, string, int) ([]messagepkg.Message, error) {
	s.recentCalls++
	return append([]messagepkg.Message(nil), s.recent...), nil
}

func (s *assembleMessageService) ListVisibleFromBySession(context.Context, string, string) ([]messagepkg.Message, error) {
	return append([]messagepkg.Message(nil), s.visible...), nil
}

func (*assembleMessageService) GetByIDBySession(context.Context, string, string) (messagepkg.Message, error) {
	return messagepkg.Message{}, nil
}

var assembleBase = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// assembleRow is a stored row as the loaders hand it over: content as JSON,
// metadata undecoded.
func assembleRow(t *testing.T, id string, message sdk.Message, at time.Time, metadata map[string]any) messagepkg.Message {
	t.Helper()
	row := admissionHistoryMessage(t, id, message, metadata)
	row.CreatedAt = at
	return row
}

func assembleText(t *testing.T, id, role, text string, at time.Time) messagepkg.Message {
	t.Helper()
	message := sdk.UserMessage(text)
	if role == "assistant" {
		message = sdk.AssistantMessage(text)
	}
	return assembleRow(t, id, message, at, nil)
}

func newAssembleService(messages *assembleMessageService) *Service {
	return &Service{messageService: messages, logger: slog.New(slog.DiscardHandler)}
}

func assembleRequest(purpose contextPurpose, chat ChatRequest) contextRequest {
	chat.BotID = pipelineTestBotID
	chat.ThreadID = pipelineTestSessionID
	return chatContextRequest(purpose, chat, 100000)
}

func messageTexts(messages []ModelMessage) []string {
	texts := make([]string, 0, len(messages))
	for _, message := range messages {
		texts = append(texts, strings.TrimSpace(message.TextContent()))
	}
	return texts
}

func assertTexts(t *testing.T, got []ModelMessage, want ...string) {
	t.Helper()
	texts := messageTexts(got)
	if len(texts) != len(want) {
		t.Fatalf("messages = %q, want %q", texts, want)
	}
	for i := range want {
		if texts[i] != want[i] {
			t.Fatalf("messages = %q, want %q", texts, want)
		}
	}
}

func preflightFailure() map[string]any {
	return map[string]any{messagepkg.HistoryFailureOriginMetadataKey: messagepkg.HistoryFailureOriginUserMessageHook}
}

// The rows after the compaction boundary are all there is: nothing is dropped
// by age, and an input rejected before the agent saw it never reaches the
// model, whichever purpose reads the history.
func TestAssembleContextSourcesAndPreflightFailures(t *testing.T) {
	t.Parallel()

	old := assembleBase.Add(-25 * time.Hour)
	rows := []messagepkg.Message{
		assembleText(t, "old-question", "user", "question from yesterday", old),
		assembleText(t, "old-answer", "assistant", "answer from yesterday", old.Add(time.Minute)),
		assembleRow(t, "rejected", sdk.UserMessage("rejected input"), assembleBase.Add(-time.Hour), preflightFailure()),
		assembleRow(t, "rejected-reply", sdk.AssistantMessage("rejected reply"), assembleBase.Add(-time.Hour+time.Second), preflightFailure()),
		assembleText(t, "question", "user", "latest question", assembleBase),
	}
	var users []messagepkg.Message
	for _, row := range rows {
		if row.Role == "user" {
			users = append(users, row)
		}
	}
	cases := []struct {
		name    string
		purpose contextPurpose
		want    []string
		loader  func(*assembleMessageService) (loadedAfter, loadedRecent int)
	}{
		{
			name: "run", purpose: contextPurposeRun,
			want: []string{"question from yesterday", "answer from yesterday", "latest question"},
		},
		{
			name: "continuation", purpose: contextPurposeContinuation,
			want: []string{"question from yesterday", "answer from yesterday", "latest question"},
		},
		{
			name: "memory query", purpose: contextPurposeMemoryQuery,
			want: []string{"question from yesterday", "latest question"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			source := &assembleMessageService{after: rows, recent: users}
			assembled, err := newAssembleService(source).assembleContext(context.Background(), assembleRequest(tc.purpose, ChatRequest{}))
			if err != nil {
				t.Fatalf("assembleContext: %v", err)
			}
			records := make([]ModelMessage, 0, len(assembled.Records))
			for _, record := range assembled.Records {
				records = append(records, record.ModelMessage)
			}
			assertTexts(t, records, tc.want...)
			if tc.purpose == contextPurposeMemoryQuery {
				if source.afterCalls != 0 || source.recentCalls != 1 {
					t.Fatalf("memory query loaded %d full histories and %d recent reads, want only the recent read", source.afterCalls, source.recentCalls)
				}
				return
			}
			assertTexts(t, assembled.Messages, tc.want...)
			if source.afterCalls != 1 || source.recentCalls != 0 {
				t.Fatalf("loaded %d full histories and %d recent reads, want one full history", source.afterCalls, source.recentCalls)
			}
		})
	}
}

// A request without a session has no stored history to read.
func TestAssembleContextWithoutSessionHasNoHistory(t *testing.T) {
	t.Parallel()

	source := &assembleMessageService{after: []messagepkg.Message{assembleText(t, "u", "user", "hello", assembleBase)}}
	request := assembleRequest(contextPurposeRun, ChatRequest{})
	request.Scope.SessionID = ""
	assembled, err := newAssembleService(source).assembleContext(context.Background(), request)
	if err != nil {
		t.Fatalf("assembleContext: %v", err)
	}
	if len(assembled.Messages) != 0 || source.afterCalls != 0 {
		t.Fatalf("assembled %d messages after %d loads, want none", len(assembled.Messages), source.afterCalls)
	}
}

// A summary stands where the rows it replaced started: before everything
// when it predates the surviving rows, between them otherwise.
func TestAssembleContextPlacesSummaryAtItsAnchor(t *testing.T) {
	t.Parallel()

	rows := []messagepkg.Message{
		assembleText(t, "first", "user", "first", assembleBase.Add(time.Minute)),
		assembleText(t, "second", "assistant", "second", assembleBase.Add(3*time.Minute)),
		assembleText(t, "third", "user", "third", assembleBase.Add(4*time.Minute)),
	}
	cases := []struct {
		name   string
		anchor time.Time
		want   []string
	}{
		{"before every surviving row", assembleBase, []string{"<summary>\nearlier window\n</summary>", "first", "second", "third"}},
		{"between surviving rows", assembleBase.Add(2 * time.Minute), []string{"first", "<summary>\nearlier window\n</summary>", "second", "third"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			service := newAssembleService(&assembleMessageService{after: rows})
			service.queries = fakeArtifactLineageQueries{rows: []sqlc.BotHistoryMessageCompact{
				compactionLogRow(t, "earlier window", "covered-message", tc.anchor.UnixMilli()),
			}}
			assembled, err := service.assembleContext(context.Background(), assembleRequest(contextPurposeRun, ChatRequest{}))
			if err != nil {
				t.Fatalf("assembleContext: %v", err)
			}
			assertTexts(t, assembled.Messages, tc.want...)
			if len(historyContextFragsForMessages(assembled.Messages, assembled.Records)) != 1 {
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
		assembleText(t, "u1", "user", "earlier question", assembleBase),
		assembleText(t, "a1", "assistant", "earlier answer", assembleBase.Add(time.Minute)),
		assembleText(t, "u2", "user", "retried question", assembleBase.Add(2*time.Minute)),
		assembleText(t, "a2", "assistant", "replaced answer", assembleBase.Add(3*time.Minute)),
	}
	t.Run("cut at the replaced turn", func(t *testing.T) {
		t.Parallel()

		assembled, err := newAssembleService(&assembleMessageService{after: rows}).assembleContext(context.Background(),
			assembleRequest(contextPurposeRun, ChatRequest{HistoryCutoffBeforeMessageID: "u2"}))
		if err != nil {
			t.Fatalf("assembleContext: %v", err)
		}
		assertTexts(t, assembled.Messages, "earlier question", "earlier answer")
	})
	t.Run("replayed turn already covered by a summary", func(t *testing.T) {
		t.Parallel()

		// The surviving rows all come after the covered turn, so a cut at it
		// leaves nothing before it but the request row itself.
		source := &assembleMessageService{after: rows[3:4], visible: rows[2:4]}
		assembled, err := newAssembleService(source).assembleContext(context.Background(),
			assembleRequest(contextPurposeRun, ChatRequest{HistoryCutoffBeforeMessageID: "a2", RequiredHistoryMessageID: "u2"}))
		if err != nil {
			t.Fatalf("assembleContext: %v", err)
		}
		assertTexts(t, assembled.Messages, "retried question")
		if last := assembled.Records[len(assembled.Records)-1]; !last.Required || last.DBMessageID != "u2" {
			t.Fatalf("last record = %#v, want the required request row", last)
		}
	})
}

// A request that already stored its own row must not meet that row again in
// the history: by row id when it has one, by the external message id otherwise.
func TestAssembleContextDedupesThePersistedCurrentRow(t *testing.T) {
	t.Parallel()

	current := assembleText(t, "row-current", "user", "continue the task", assembleBase.Add(time.Minute))
	current.ExternalMessageID = "external-current"
	rows := []messagepkg.Message{
		assembleText(t, "row-earlier", "user", "earlier", assembleBase),
		current,
	}
	cases := []struct {
		name string
		chat ChatRequest
		want []string
	}{
		{"by row id without an external id", ChatRequest{UserMessagePersisted: true, PersistedUserMessageID: "row-current"}, []string{"earlier"}},
		{"by external id without a row id", ChatRequest{UserMessagePersisted: true, ExternalMessageID: "external-current"}, []string{"earlier"}},
		{"row id the history does not hold", ChatRequest{UserMessagePersisted: true, PersistedUserMessageID: "row-elsewhere"}, []string{"earlier", "continue the task"}},
		{"request that persisted nothing", ChatRequest{PersistedUserMessageID: "row-current"}, []string{"earlier", "continue the task"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assembled, err := newAssembleService(&assembleMessageService{after: rows}).assembleContext(context.Background(), assembleRequest(contextPurposeRun, tc.chat))
			if err != nil {
				t.Fatalf("assembleContext: %v", err)
			}
			assertTexts(t, assembled.Messages, tc.want...)
		})
	}
}

// Old tool exchanges and reasoning stay in the history exactly as stored; the
// provider decides what to replay of them.
func TestAssembleContextKeepsToolExchangesAndReasoning(t *testing.T) {
	t.Parallel()

	reasoning := sdk.ReasoningPart{Text: "old thinking", ProviderMetadata: partmeta.Fold(map[string]any{"anthropic": map[string]any{"signature": "SIG_OLD"}})}
	var rows []messagepkg.Message
	at := assembleBase
	add := func(id string, message sdk.Message) {
		rows = append(rows, assembleRow(t, id, message, at, nil))
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

	assembled, err := newAssembleService(&assembleMessageService{after: rows}).assembleContext(context.Background(), assembleRequest(contextPurposeRun, ChatRequest{}))
	if err != nil {
		t.Fatalf("assembleContext: %v", err)
	}
	messages := normalizeContextMessages(assembled.Messages)
	if len(messages) != len(rows) {
		t.Fatalf("messages = %d, want all %d stored rows", len(messages), len(rows))
	}
	for i, row := range rows {
		var stored ModelMessage
		if err := json.Unmarshal(row.Content, &stored); err != nil {
			t.Fatal(err)
		}
		got, _ := json.Marshal(modelMessageToSDKMessage(messages[i]))
		want, _ := json.Marshal(modelMessageToSDKMessage(stored))
		if string(got) != string(want) {
			t.Fatalf("message %d changed:\ngot  %s\nwant %s", i, got, want)
		}
	}
	var sawReasoning, sawResult bool
	for _, part := range modelMessageToSDKMessage(messages[1]).Content {
		if _, ok := part.(sdk.ReasoningPart); ok {
			sawReasoning = true
		}
	}
	for _, part := range modelMessageToSDKMessage(messages[2]).Content {
		if _, ok := part.(sdk.ToolResultPart); ok {
			sawResult = true
		}
	}
	if !sawReasoning || !sawResult {
		t.Fatalf("old turn lost its reasoning (%t) or its tool result (%t)", sawReasoning, sawResult)
	}
}

// A load that reaches its byte budget may have stopped short of the oldest
// row; the assembly says so, and a load that fits does not.
func TestAssembleContextFlagsALoadThatReachedItsBudget(t *testing.T) {
	t.Parallel()

	rows := []messagepkg.Message{
		assembleText(t, "u1", "user", strings.Repeat("x", 4000), assembleBase),
		assembleText(t, "u2", "user", strings.Repeat("y", 4000), assembleBase.Add(time.Minute)),
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

			source := &assembleMessageService{after: rows}
			request := assembleRequest(contextPurposeRun, ChatRequest{})
			request.Budget = tc.budget
			assembled, err := newAssembleService(source).assembleContext(context.Background(), request)
			if err != nil {
				t.Fatalf("assembleContext: %v", err)
			}
			if assembled.Truncated != tc.want {
				t.Fatalf("Truncated = %t, want %t", assembled.Truncated, tc.want)
			}
			if want := int64(tc.budget) * contextfrag.EstimateBytesPerToken; source.maxBytes != want {
				t.Fatalf("load budget = %d bytes, want %d", source.maxBytes, want)
			}
		})
	}
}

// Normalizing keeps every tool exchange and closes a call left open.
func TestNormalizeContextMessagesKeepsToolExchanges(t *testing.T) {
	t.Parallel()

	var messages []ModelMessage
	messages = append(messages, ModelMessage{Content: newTextContent("missing role")})
	for i := range 6 {
		call := "call-" + string(rune('a'+i))
		messages = append(messages, sdkMessagesToModelMessages([]sdk.Message{
			sdk.UserMessage("question"),
			{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{sdk.ToolCallPart{ToolCallID: call, ToolName: "web_fetch"}}},
			sdk.ToolMessage(sdk.ToolResultPart{ToolCallID: call, ToolName: "web_fetch", Result: toolexec.OutputFromValue("body")}),
		})...)
	}
	messages = append(messages, sdkMessagesToModelMessages([]sdk.Message{
		{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{sdk.ToolCallPart{ToolCallID: "open-call", ToolName: "ask_user"}}},
	})...)

	got := normalizeContextMessages(messages)
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
