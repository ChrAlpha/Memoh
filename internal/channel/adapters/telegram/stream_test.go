package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	tele "gopkg.in/telebot.v4"

	"github.com/felinics/memoh/internal/channel"
)

func mustPreparedTelegramEvent(t *testing.T, event channel.StreamEvent) channel.PreparedStreamEvent {
	t.Helper()
	prepared, err := channel.PrepareStreamEvent(context.Background(), nil, channel.ChannelConfig{
		BotID:       "bot-test",
		ChannelType: Type,
	}, event)
	if err != nil {
		t.Fatalf("prepare telegram stream event: %v", err)
	}
	return prepared
}

// drive delivers an event the way the manager does: a final ends the preview
// with Finish, a completed status ends it with no final, and anything else
// updates it.
func (s *telegramPreview) drive(ctx context.Context, event channel.PreparedStreamEvent) error {
	switch {
	case event.Type == channel.StreamEventFinal && event.Final != nil:
		return s.Finish(ctx, &event)
	case event.Type == channel.StreamEventStatus && event.Status == channel.StreamStatusCompleted:
		return s.Finish(ctx, nil)
	default:
		return s.Update(ctx, event)
	}
}

func TestTelegramPreview_AbortNil(t *testing.T) {
	t.Parallel()

	var s *telegramPreview
	if err := s.Abort(context.Background()); err != nil {
		t.Fatalf("Abort on nil preview should return nil: %v", err)
	}
}

func TestTelegramPreview_UpdateClosed(t *testing.T) {
	t.Parallel()

	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{adapter: adapter}
	s.closed.Store(true)

	ctx := context.Background()
	err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "x"}))
	if err == nil {
		t.Fatal("Update on a closed preview should return error")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected closed error: %v", err)
	}
}

func TestTelegramOutboundStream_PushStatusNoOp(t *testing.T) {
	t.Parallel()

	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{adapter: adapter}

	ctx := context.Background()
	err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{Type: channel.StreamEventStatus}))
	if err != nil {
		t.Fatalf("StreamEventStatus should be no-op: %v", err)
	}
}

func TestTelegramOutboundStream_PushNilAdapter(t *testing.T) {
	t.Parallel()

	s := &telegramPreview{adapter: nil}
	ctx := context.Background()
	err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "x"}))
	if err == nil {
		t.Fatal("Push with nil adapter should return error")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("expected not configured error: %v", err)
	}
}

func TestTelegramOutboundStream_PushUnknownEventTypeSkipped(t *testing.T) {
	t.Parallel()

	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{adapter: adapter}
	ctx := context.Background()

	err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{Type: channel.StreamEventType("unknown")}))
	if err != nil {
		t.Fatalf("Push with unknown event type should be silently skipped: %v", err)
	}
}

func TestTelegramOutboundStream_PushEmptyDeltaNoOp(t *testing.T) {
	t.Parallel()

	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{adapter: adapter}
	ctx := context.Background()

	err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{Type: channel.StreamEventDelta, Delta: ""}))
	if err != nil {
		t.Fatalf("empty delta should be no-op: %v", err)
	}
}

func TestTelegramPreview_UpdateErrorReply(t *testing.T) {
	var sent []string
	origGetBot := getOrCreateBotForTest
	origSendText := sendTextForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return &tele.Bot{Token: "fake"}, nil
	}
	sendTextForTest = func(_ *tele.Bot, _ string, text string, _ int, _ string) (int64, int, error) {
		sent = append(sent, text)
		return 1, 1, nil
	}
	t.Cleanup(func() {
		getOrCreateBotForTest = origGetBot
		sendTextForTest = origSendText
	})

	cases := []struct {
		name  string
		event channel.StreamEvent
		want  []string
	}{
		{
			name:  "coded error shows the copy as it is",
			event: channel.StreamEvent{Type: channel.StreamEventError, Error: "The workspace is unreachable.", ErrorCode: "workspace.unreachable"},
			want:  []string{"The workspace is unreachable."},
		},
		{
			name:  "uncoded error is labelled",
			event: channel.StreamEvent{Type: channel.StreamEventError, Error: "request failed"},
			want:  []string{"Error: request failed"},
		},
		{
			name:  "blank error sends nothing",
			event: channel.StreamEvent{Type: channel.StreamEventError, Error: "  "},
			want:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sent = nil
			adapter := NewTelegramAdapter(nil)
			preview, err := adapter.OpenPreview(context.Background(), channel.ChannelConfig{
				ID:          "cfg-1",
				Credentials: map[string]any{"botToken": "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ"},
			}, "12345", channel.StreamOptions{Metadata: map[string]any{"conversation_type": "private"}})
			if err != nil {
				t.Fatalf("open preview: %v", err)
			}
			if err := preview.Update(context.Background(), mustPreparedTelegramEvent(t, tc.event)); err != nil {
				t.Fatalf("update with error event: %v", err)
			}
			if len(sent) != len(tc.want) || strings.Join(sent, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("sent messages = %q, want %q", sent, tc.want)
			}
		})
	}
}

func TestTelegramPreview_AbortIgnoresCanceledContext(t *testing.T) {
	t.Parallel()

	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{adapter: adapter}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := s.Abort(ctx); err != nil {
		t.Fatalf("Abort must not fail: %v", err)
	}
	if err := s.Update(context.Background(), mustPreparedTelegramEvent(t, channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "x"})); err == nil {
		t.Fatal("an aborted preview shows nothing more")
	}
}

// Test editStreamMessage dedup: no API call when content equals lastEdited (avoids Telegram "message is not modified" error).
func TestEditStreamMessage_NoEditWhenSameContent(t *testing.T) {
	t.Parallel()

	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter:      adapter,
		streamChatID: 1,
		streamMsgID:  1,
		lastEdited:   "hello",
		lastEditedAt: time.Now().Add(-time.Minute),
	}
	ctx := context.Background()

	tests := []struct {
		name string
		text string
	}{
		{"exact same", "hello"},
		{"trimmed same", "  hello  "},
		{"leading space", " hello"},
		{"trailing space", "hello "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.editStreamMessage(ctx, tt.text)
			if err != nil {
				t.Fatalf("editStreamMessage(same content) should return nil to avoid duplicate edit API call: %v", err)
			}
		})
	}
}

func TestEditStreamMessage_NoEditWhenMessageNotSent(t *testing.T) {
	t.Parallel()

	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{adapter: adapter, streamMsgID: 0}
	ctx := context.Background()

	err := s.editStreamMessage(ctx, "any")
	if err != nil {
		t.Fatalf("editStreamMessage when streamMsgID==0 should return nil: %v", err)
	}
}

func TestEditStreamMessage_NoEditWhenThrottled(t *testing.T) {
	t.Parallel()

	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter:      adapter,
		streamChatID: 1,
		streamMsgID:  1,
		lastEdited:   "a",
		lastEditedAt: time.Now(), // just now, within throttle window
	}
	ctx := context.Background()

	err := s.editStreamMessage(ctx, "ab")
	if err != nil {
		t.Fatalf("editStreamMessage within throttle window should skip edit and return nil: %v", err)
	}
}

func TestEditStreamMessage_429SetsBackoffAndReturnsNil(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	before := time.Now().Add(-time.Minute)
	s := &telegramPreview{
		adapter:      adapter,
		cfg:          channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		streamChatID: 1,
		streamMsgID:  1,
		lastEdited:   "a",
		lastEditedAt: before,
	}
	ctx := context.Background()

	origGetBot := getOrCreateBotForTest
	origEdit := testEditFunc
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return &tele.Bot{Token: "fake"}, nil
	}
	testEditFunc = func(*tele.Bot, int64, int, string, string) error {
		return tele.FloodError{RetryAfter: 2}
	}
	defer func() {
		getOrCreateBotForTest = origGetBot
		testEditFunc = origEdit
	}()

	err := s.editStreamMessage(ctx, "b")
	if err != nil {
		t.Fatalf("editStreamMessage on 429 should return nil (backoff): %v", err)
	}
	s.mu.Lock()
	lastEdited := s.lastEdited
	lastEditedAt := s.lastEditedAt
	s.mu.Unlock()
	if lastEdited != "a" {
		t.Fatalf("on 429 lastEdited should remain unchanged: got %q", lastEdited)
	}
	if !lastEditedAt.After(before) {
		t.Fatalf("on 429 lastEditedAt should be pushed forward for backoff: got %v", lastEditedAt)
	}
}

func TestEditStreamMessageFinal_Success(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter:      adapter,
		cfg:          channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		streamChatID: 1,
		streamMsgID:  1,
		lastEdited:   "a",
		lastEditedAt: time.Now().Add(-time.Minute),
	}
	ctx := context.Background()

	origGetBot := getOrCreateBotForTest
	origEdit := testEditFunc
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return &tele.Bot{Token: "fake"}, nil
	}
	testEditFunc = func(*tele.Bot, int64, int, string, string) error {
		return nil
	}
	defer func() {
		getOrCreateBotForTest = origGetBot
		testEditFunc = origEdit
	}()

	err := s.editStreamMessageFinal(ctx, "final text")
	if err != nil {
		t.Fatalf("editStreamMessageFinal should succeed: %v", err)
	}
	s.mu.Lock()
	lastEdited := s.lastEdited
	s.mu.Unlock()
	if lastEdited != "final text" {
		t.Fatalf("expected lastEdited to be updated: got %q", lastEdited)
	}
}

// TestEditStreamMessageFinal_UnrecoverableFallsBackToNewMessage pins the
// recovery path: when the streamed placeholder is gone/uneditable, the final
// edit can never land, so instead of silently dropping the answer (the old
// no-op-on-unrecoverable behavior) the stream posts it as a NEW message.
func TestEditStreamMessageFinal_UnrecoverableFallsBackToNewMessage(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter:      adapter,
		cfg:          channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		target:       "123",
		streamChatID: 1,
		streamMsgID:  1,
		lastEdited:   "a",
		lastEditedAt: time.Now().Add(-time.Minute),
	}
	ctx := context.Background()

	origGetBot := getOrCreateBotForTest
	origEdit := testEditFunc
	origSendText := sendTextForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return &tele.Bot{Token: "fake"}, nil
	}
	testEditFunc = func(*tele.Bot, int64, int, string, string) error {
		return &tele.Error{Code: 400, Description: "Bad Request: message to edit not found"}
	}
	var sentText string
	var sentCount int
	sendTextForTest = func(_ *tele.Bot, _ string, text string, _ int, _ string) (int64, int, error) {
		sentText = text
		sentCount++
		return 1, 99, nil
	}
	defer func() {
		getOrCreateBotForTest = origGetBot
		testEditFunc = origEdit
		sendTextForTest = origSendText
	}()

	if err := s.editStreamMessageFinal(ctx, "final answer"); err != nil {
		t.Fatalf("unrecoverable edit should recover via a new message, got error: %v", err)
	}
	if sentCount != 1 || sentText != "final answer" {
		t.Fatalf("expected the final answer posted as one new message, got count=%d text=%q", sentCount, sentText)
	}
}

func TestEditStreamMessageFinal_SameContentNoOp(t *testing.T) {
	t.Parallel()

	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter:      adapter,
		streamChatID: 1,
		streamMsgID:  1,
		lastEdited:   "same",
		lastEditedAt: time.Now(),
	}
	ctx := context.Background()

	err := s.editStreamMessageFinal(ctx, "same")
	if err != nil {
		t.Fatalf("editStreamMessageFinal with same content should return nil: %v", err)
	}
}

func TestEditStreamMessageFinal_NoMessageNoOp(t *testing.T) {
	t.Parallel()

	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{adapter: adapter, streamMsgID: 0}
	ctx := context.Background()

	err := s.editStreamMessageFinal(ctx, "any")
	if err != nil {
		t.Fatalf("editStreamMessageFinal when streamMsgID==0 should return nil: %v", err)
	}
}

// --- Draft mode (sendMessageDraft) tests ---

func TestSendDraft_ThrottleSkip(t *testing.T) {
	t.Parallel()

	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter:       adapter,
		cfg:           channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		isPrivateChat: true,
		draftID:       1,
		streamChatID:  123,
		lastEditedAt:  time.Now(), // just now, within draft throttle window
	}
	ctx := context.Background()

	err := s.sendDraft(ctx, "hello")
	if err != nil {
		t.Fatalf("sendDraft within throttle window should skip and return nil: %v", err)
	}
}

func TestSendDraft_EmptyTextSkip(t *testing.T) {
	t.Parallel()

	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter:       adapter,
		isPrivateChat: true,
		draftID:       1,
		streamChatID:  123,
		lastEditedAt:  time.Now().Add(-time.Minute),
	}
	ctx := context.Background()

	err := s.sendDraft(ctx, "   ")
	if err != nil {
		t.Fatalf("sendDraft with whitespace-only text should skip and return nil: %v", err)
	}
}

func TestSendDraft_Success(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter:       adapter,
		cfg:           channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		isPrivateChat: true,
		draftID:       1,
		streamChatID:  123,
		lastEditedAt:  time.Now().Add(-time.Minute),
	}
	ctx := context.Background()

	origGetBot := getOrCreateBotForTest
	origDraft := sendDraftForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return &tele.Bot{Token: "fake"}, nil
	}
	var capturedChatID int64
	var capturedDraftID int
	var capturedText string
	sendDraftForTest = func(_ *tele.Bot, chatID int64, draftID int, text string, _ string) error {
		capturedChatID = chatID
		capturedDraftID = draftID
		capturedText = text
		return nil
	}
	defer func() {
		getOrCreateBotForTest = origGetBot
		sendDraftForTest = origDraft
	}()

	err := s.sendDraft(ctx, "streaming text")
	if err != nil {
		t.Fatalf("sendDraft should succeed: %v", err)
	}
	if capturedChatID != 123 {
		t.Fatalf("expected chatID 123, got %d", capturedChatID)
	}
	if capturedDraftID != 1 {
		t.Fatalf("expected draftID 1, got %d", capturedDraftID)
	}
	if capturedText != "streaming text" {
		t.Fatalf("expected text 'streaming text', got %q", capturedText)
	}
}

func TestSendDraft_429Backoff(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	before := time.Now().Add(-time.Minute)
	s := &telegramPreview{
		adapter:       adapter,
		cfg:           channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		isPrivateChat: true,
		draftID:       1,
		streamChatID:  123,
		lastEditedAt:  before,
	}
	ctx := context.Background()

	origGetBot := getOrCreateBotForTest
	origDraft := sendDraftForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return &tele.Bot{Token: "fake"}, nil
	}
	sendDraftForTest = func(*tele.Bot, int64, int, string, string) error {
		return tele.FloodError{RetryAfter: 2}
	}
	defer func() {
		getOrCreateBotForTest = origGetBot
		sendDraftForTest = origDraft
	}()

	err := s.sendDraft(ctx, "hello")
	if err != nil {
		t.Fatalf("sendDraft on 429 should return nil (backoff): %v", err)
	}
	s.mu.Lock()
	lastEditedAt := s.lastEditedAt
	s.mu.Unlock()
	if !lastEditedAt.After(before) {
		t.Fatalf("on 429 lastEditedAt should be pushed forward for backoff")
	}
}

func TestDraftMode_DeltaUsesSendDraft(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter:       adapter,
		cfg:           channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		isPrivateChat: true,
		draftID:       1,
		streamChatID:  123,
	}
	ctx := context.Background()

	origGetBot := getOrCreateBotForTest
	origDraft := sendDraftForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return &tele.Bot{Token: "fake"}, nil
	}
	draftCalls := 0
	sendDraftForTest = func(*tele.Bot, int64, int, string, string) error {
		draftCalls++
		return nil
	}
	defer func() {
		getOrCreateBotForTest = origGetBot
		sendDraftForTest = origDraft
	}()

	err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "Hello "}))
	if err != nil {
		t.Fatalf("Push delta should succeed: %v", err)
	}
	if draftCalls != 1 {
		t.Fatalf("expected 1 sendDraft call, got %d", draftCalls)
	}
	s.mu.Lock()
	buf := s.buf.String()
	s.mu.Unlock()
	if buf != "Hello " {
		t.Fatalf("expected buffer to be 'Hello ', got %q", buf)
	}
}

// TestDraftMode_ResetStartsDraftFromEmpty verifies that after a reset (the
// agent retrying a failed attempt) the draft shows only the regenerated text.
func TestDraftMode_ResetStartsDraftFromEmpty(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter:       adapter,
		cfg:           channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		isPrivateChat: true,
		draftID:       1,
		streamChatID:  123,
	}
	ctx := context.Background()

	origGetBot := getOrCreateBotForTest
	origDraft := sendDraftForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return &tele.Bot{Token: "fake"}, nil
	}
	var drafts []string
	sendDraftForTest = func(_ *tele.Bot, _ int64, _ int, text string, _ string) error {
		drafts = append(drafts, text)
		return nil
	}
	defer func() {
		getOrCreateBotForTest = origGetBot
		sendDraftForTest = origDraft
	}()

	for _, event := range []channel.StreamEvent{
		{Type: channel.StreamEventDelta, Delta: "Hello from the "},
		{Type: channel.StreamEventReset},
		{Type: channel.StreamEventDelta, Delta: "Hello "},
	} {
		// Let every delta through the draft throttle.
		s.mu.Lock()
		s.lastEditedAt = time.Time{}
		s.mu.Unlock()
		if err := s.drive(ctx, mustPreparedTelegramEvent(t, event)); err != nil {
			t.Fatalf("push %s: %v", event.Type, err)
		}
	}

	want := []string{"Hello from the ", "Hello "}
	if !reflect.DeepEqual(drafts, want) {
		t.Fatalf("drafts = %q, want %q", drafts, want)
	}
}

// TestEditMode_ResetEditsSameMessageFromEmpty verifies that in group chats a
// reset keeps the preview message and the regenerated text replaces the failed
// attempt's text in it.
func TestEditMode_ResetEditsSameMessageFromEmpty(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter:      adapter,
		cfg:          channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		target:       "123",
		streamChatID: 42,
		streamMsgID:  7,
	}
	s.buf.WriteString("Hello from the ")
	ctx := context.Background()

	// Answers any Telegram API call the stream makes.
	bot := newTestTelegramBot(telegramRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)),
		}, nil
	}))
	origGetBot := getOrCreateBotForTest
	origSendText := sendTextForTest
	origEdit := testEditFunc
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return bot, nil
	}
	sends := 0
	sendTextForTest = func(*tele.Bot, string, string, int, string) (int64, int, error) {
		sends++
		return 42, 8, nil
	}
	var editedIDs []int
	var edits []string
	testEditFunc = func(_ *tele.Bot, _ int64, msgID int, text string, _ string) error {
		editedIDs = append(editedIDs, msgID)
		edits = append(edits, text)
		return nil
	}
	defer func() {
		getOrCreateBotForTest = origGetBot
		sendTextForTest = origSendText
		testEditFunc = origEdit
	}()

	if err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{Type: channel.StreamEventReset})); err != nil {
		t.Fatalf("push reset: %v", err)
	}
	if err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "Hello "})); err != nil {
		t.Fatalf("push delta: %v", err)
	}

	if sends != 0 {
		t.Fatalf("reset must not open a new preview message, got %d sends", sends)
	}
	if len(edits) != 1 || editedIDs[0] != 7 || normalizeStreamComparableText(edits[0]) != "Hello" {
		t.Fatalf("edits = %q on %v, want one edit of message 7 to %q", edits, editedIDs, "Hello")
	}
}

func TestDraftMode_PhaseEndTextIsNoOp(t *testing.T) {
	t.Parallel()

	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter:       adapter,
		isPrivateChat: true,
		draftID:       1,
		streamChatID:  123,
	}
	s.buf.WriteString("some content")
	ctx := context.Background()

	err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{
		Type:  channel.StreamEventPhaseEnd,
		Phase: channel.StreamPhaseText,
	}))
	if err != nil {
		t.Fatalf("PhaseEnd in draft mode should be no-op: %v", err)
	}
}

// Render stripping is covered in ask_user_test.go (TestRenderAskUserChoiceShowsOnlyQuestion).

// TestToolCallFlow_ThreeMessagesPerCall verifies that a single tool call
// combined with pre-existing streamed text produces three distinct messages:
// (1) flush of buffered pre-text, (2) running state for the tool call,
// (3) completed / failed state for the tool call. The streaming state must be
// reset between the flush and the start, then tool_call_end edits the running
// message in place so one tool call produces exactly one tool-call message.
func TestToolCallFlow_FlushPreTextAndEditRunning(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter:      adapter,
		cfg:          channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		target:       "123",
		streamChatID: 42,
		streamMsgID:  7,
	}
	s.buf.WriteString("pre-tool text")
	ctx := context.Background()

	origGetBot := getOrCreateBotForTest
	origSendText := sendTextForTest
	origEdit := testEditFunc
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return &tele.Bot{Token: "fake"}, nil
	}
	var sentTexts []string
	var msgIDCounter int
	sendTextForTest = func(_ *tele.Bot, _ string, text string, _ int, _ string) (int64, int, error) {
		sentTexts = append(sentTexts, text)
		msgIDCounter++
		return 42, msgIDCounter, nil
	}
	var editTexts []string
	testEditFunc = func(_ *tele.Bot, _ int64, _ int, text string, _ string) error {
		editTexts = append(editTexts, text)
		return nil
	}
	defer func() {
		getOrCreateBotForTest = origGetBot
		sendTextForTest = origSendText
		testEditFunc = origEdit
	}()

	tcStart := &channel.StreamToolCall{Name: "read", CallID: "call_1", Input: map[string]any{"path": "/tmp/a"}}
	tcEnd := &channel.StreamToolCall{Name: "read", CallID: "call_1", Input: map[string]any{"path": "/tmp/a"}, Result: map[string]any{"ok": true}}

	if err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{Type: channel.StreamEventToolCallStart, ToolCall: tcStart})); err != nil {
		t.Fatalf("push start: %v", err)
	}

	s.mu.Lock()
	streamMsgAfterStart := s.streamMsgID
	bufAfterStart := s.buf.String()
	s.mu.Unlock()
	if streamMsgAfterStart != 0 {
		t.Fatalf("streamMsgID should be reset after tool_call_start, got %d", streamMsgAfterStart)
	}
	if bufAfterStart != "" {
		t.Fatalf("buf should be reset after tool_call_start, got %q", bufAfterStart)
	}

	if err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{Type: channel.StreamEventToolCallEnd, ToolCall: tcEnd})); err != nil {
		t.Fatalf("push end: %v", err)
	}

	// Edits: 1 for pre-text flush, 1 for running → completed.
	if len(editTexts) != 2 {
		t.Fatalf("expected exactly 2 edits (pre-text flush + running→completed), got %d: %v", len(editTexts), editTexts)
	}
	if !strings.Contains(editTexts[0], "pre-tool text") {
		t.Fatalf("first edit should be the pre-text flush: %q", editTexts[0])
	}
	if !strings.Contains(editTexts[1], "completed") {
		t.Fatalf("second edit should flip the tool call to completed: %q", editTexts[1])
	}
	if len(sentTexts) != 1 {
		t.Fatalf("expected exactly 1 send (running), got %d: %v", len(sentTexts), sentTexts)
	}
	if !strings.Contains(sentTexts[0], "running") {
		t.Fatalf("only send should be the running state: %q", sentTexts[0])
	}
}

// TestToolCallFlow_NoPreTextEditsRunningInPlace verifies that when no text
// stream is active, tool_call_start sends the running message and
// tool_call_end edits it in place — no pre-text flush, one send, one edit.
func TestToolCallFlow_NoPreTextEditsRunningInPlace(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter:      adapter,
		cfg:          channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		target:       "123",
		streamChatID: 42,
	}
	ctx := context.Background()

	origGetBot := getOrCreateBotForTest
	origSendText := sendTextForTest
	origEdit := testEditFunc
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return &tele.Bot{Token: "fake"}, nil
	}
	var sentTexts []string
	var msgIDCounter int
	sendTextForTest = func(_ *tele.Bot, _ string, text string, _ int, _ string) (int64, int, error) {
		sentTexts = append(sentTexts, text)
		msgIDCounter++
		return 42, msgIDCounter, nil
	}
	var editTexts []string
	testEditFunc = func(_ *tele.Bot, _ int64, _ int, text string, _ string) error {
		editTexts = append(editTexts, text)
		return nil
	}
	defer func() {
		getOrCreateBotForTest = origGetBot
		sendTextForTest = origSendText
		testEditFunc = origEdit
	}()

	tcStart := &channel.StreamToolCall{Name: "read", CallID: "call_1", Input: map[string]any{"path": "/tmp/a"}}
	tcEnd := &channel.StreamToolCall{Name: "read", CallID: "call_1", Result: map[string]any{"ok": true}}

	if err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{Type: channel.StreamEventToolCallStart, ToolCall: tcStart})); err != nil {
		t.Fatalf("push start: %v", err)
	}
	if err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{Type: channel.StreamEventToolCallEnd, ToolCall: tcEnd})); err != nil {
		t.Fatalf("push end: %v", err)
	}

	if len(sentTexts) != 1 {
		t.Fatalf("expected exactly 1 send (running), got %d: %v", len(sentTexts), sentTexts)
	}
	if !strings.Contains(sentTexts[0], "running") {
		t.Fatalf("only send should be the running state: %q", sentTexts[0])
	}
	if len(editTexts) != 1 {
		t.Fatalf("expected exactly 1 edit (running→completed), got %d: %v", len(editTexts), editTexts)
	}
	if !strings.Contains(editTexts[0], "completed") {
		t.Fatalf("edit should flip the tool call to completed: %q", editTexts[0])
	}
}

func TestDraftMode_ToolCallStartSendsPermanentMessage(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter:       adapter,
		cfg:           channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		target:        "123",
		isPrivateChat: true,
		draftID:       1,
		streamChatID:  123,
	}
	s.buf.WriteString("partial text")
	ctx := context.Background()

	origGetBot := getOrCreateBotForTest
	origSendEdit := sendEditForTest
	origSendText := sendTextForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return &tele.Bot{Token: "fake"}, nil
	}
	var sentText string
	sendTextForTest = func(_ *tele.Bot, _ string, text string, _ int, _ string) (int64, int, error) {
		sentText = text
		return 123, 1, nil
	}
	sendEditForTest = func(_ *tele.Bot, _ int64, _ int, _ string, _ string) error {
		t.Error("editMessage should not be called in draft mode")
		return nil
	}
	defer func() {
		getOrCreateBotForTest = origGetBot
		sendEditForTest = origSendEdit
		sendTextForTest = origSendText
	}()

	err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{Type: channel.StreamEventToolCallStart}))
	if err != nil {
		t.Fatalf("Push ToolCallStart should succeed: %v", err)
	}
	if sentText != "partial text" {
		t.Fatalf("expected sendPermanentMessage with 'partial text', got %q", sentText)
	}

	s.mu.Lock()
	bufAfter := s.buf.String()
	chatID := s.streamChatID
	s.mu.Unlock()
	if bufAfter != "" {
		t.Fatalf("buffer should be reset after ToolCallStart: got %q", bufAfter)
	}
	// streamChatID should be preserved in draft mode
	if chatID != 123 {
		t.Fatalf("streamChatID should be preserved in draft mode: got %d", chatID)
	}
}

func TestDraftMode_FinalEmptyBufferSkipsDuplicate(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter:            adapter,
		cfg:                channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		target:             "123",
		isPrivateChat:      true,
		draftID:            1,
		streamChatID:       123,
		draftPermanentSent: true,
	}
	ctx := context.Background()

	// Simulate: buffer was already committed during ToolCallStart, so it's empty.
	// StreamEventFinal should NOT re-send the message via PlainText() fallback.
	origGetBot := getOrCreateBotForTest
	origSendText := sendTextForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return &tele.Bot{Token: "fake"}, nil
	}
	sendTextForTest = func(_ *tele.Bot, _ string, _ string, _ int, _ string) (int64, int, error) {
		t.Error("sendTelegramText should not be called when buffer is empty in draft mode")
		return 0, 0, nil
	}
	defer func() {
		getOrCreateBotForTest = origGetBot
		sendTextForTest = origSendText
	}()

	err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{
		Type: channel.StreamEventFinal,
		Final: &channel.StreamFinalizePayload{
			Message: channel.Message{Text: "already sent text"},
		},
	}))
	if err != nil {
		t.Fatalf("StreamEventFinal with empty buffer in draft mode should succeed: %v", err)
	}
}

func TestDraftMode_FinalOnlyPayloadSendsPermanentMessage(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter:       adapter,
		cfg:           channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		target:        "123",
		isPrivateChat: true,
		draftID:       1,
		streamChatID:  123,
	}
	ctx := context.Background()

	origGetBot := getOrCreateBotForTest
	origSendText := sendTextForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return &tele.Bot{Token: "fake"}, nil
	}
	var sentText string
	sendTextForTest = func(_ *tele.Bot, _ string, text string, _ int, _ string) (int64, int, error) {
		sentText = text
		return 123, 1, nil
	}
	defer func() {
		getOrCreateBotForTest = origGetBot
		sendTextForTest = origSendText
	}()

	err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{
		Type: channel.StreamEventFinal,
		Final: &channel.StreamFinalizePayload{
			Message: channel.Message{Text: "final answer"},
		},
	}))
	if err != nil {
		t.Fatalf("StreamEventFinal with final-only payload should succeed: %v", err)
	}
	if sentText != "final answer" {
		t.Fatalf("expected final-only payload to be sent permanently, got %q", sentText)
	}
	if !s.draftPermanentSent {
		t.Fatal("draftPermanentSent should be marked after final-only permanent send")
	}
}

// TestPreview_FinishEndsTheReplyOnce verifies that a second Finish, and an
// Abort after the reply has ended, add nothing to what the first Finish showed.
func TestPreview_FinishEndsTheReplyOnce(t *testing.T) {
	bot, _ := recordTelegramCalls(t)
	origGetBot := getOrCreateBotForTest
	origSendText := sendTextForTest
	origEdit := testEditFunc
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) { return bot, nil }
	var sends []string
	sendTextForTest = func(_ *tele.Bot, _ string, text string, _ int, _ string) (int64, int, error) {
		sends = append(sends, text)
		return 42, 8, nil
	}
	var edits []string
	testEditFunc = func(_ *tele.Bot, _ int64, _ int, text string, _ string) error {
		edits = append(edits, text)
		return nil
	}
	defer func() {
		getOrCreateBotForTest = origGetBot
		sendTextForTest = origSendText
		testEditFunc = origEdit
	}()

	s := &telegramPreview{
		adapter: NewTelegramAdapter(nil),
		cfg:     channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		target:  "42",
	}
	ctx := context.Background()
	for i, text := range []string{"first", "second"} {
		final := mustPreparedTelegramEvent(t, channel.StreamEvent{
			Type:  channel.StreamEventFinal,
			Final: &channel.StreamFinalizePayload{Message: channel.Message{Text: text}},
		})
		if err := s.Finish(ctx, &final); err != nil {
			t.Fatalf("Finish #%d: %v", i+1, err)
		}
	}
	if err := s.Abort(ctx); err != nil {
		t.Fatalf("Abort after Finish: %v", err)
	}

	if len(sends) != 1 || len(edits) != 1 || edits[0] != "first" {
		t.Fatalf("sends = %q, edits = %q, want the first final only", sends, edits)
	}
}

// emptyFinal is the final that ends a text segment: it carries no message, so
// the preview commits the text it has shown.
func emptyFinal(t *testing.T) channel.PreparedStreamEvent {
	t.Helper()
	return mustPreparedTelegramEvent(t, channel.StreamEvent{Type: channel.StreamEventFinal, Final: &channel.StreamFinalizePayload{}})
}

// TestDraftMode_EmptyFinalKeepsBufferedTextAsPermanentMessage verifies that in
// private chats the text streamed before a tool call is committed as a message
// of its own when the preview ends with an empty final, and the next preview
// posts the text after the tool call as another message.
func TestDraftMode_EmptyFinalKeepsBufferedTextAsPermanentMessage(t *testing.T) {
	newPreview := func() *telegramPreview {
		return &telegramPreview{
			adapter:       NewTelegramAdapter(nil),
			cfg:           channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
			target:        "123",
			isPrivateChat: true,
			draftID:       1,
			streamChatID:  123,
		}
	}
	ctx := context.Background()

	origGetBot := getOrCreateBotForTest
	origDraft := sendDraftForTest
	origSendText := sendTextForTest
	origSendEdit := sendEditForTest
	origEdit := testEditFunc
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return &tele.Bot{Token: "fake"}, nil
	}
	sendDraftForTest = func(*tele.Bot, int64, int, string, string) error { return nil }
	var sent []string
	sendTextForTest = func(_ *tele.Bot, _ string, text string, _ int, _ string) (int64, int, error) {
		sent = append(sent, text)
		return 123, len(sent), nil
	}
	sendEditForTest = func(*tele.Bot, int64, int, string, string) error {
		t.Error("draft mode must not edit messages")
		return nil
	}
	testEditFunc = func(*tele.Bot, int64, int, string, string) error {
		t.Error("draft mode must not edit messages")
		return nil
	}
	defer func() {
		getOrCreateBotForTest = origGetBot
		sendDraftForTest = origDraft
		sendTextForTest = origSendText
		sendEditForTest = origSendEdit
		testEditFunc = origEdit
	}()

	for _, text := range []string{"Let me check.", "Here is the answer."} {
		s := newPreview()
		if err := s.Update(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{Type: channel.StreamEventDelta, Delta: text, Phase: channel.StreamPhaseText})); err != nil {
			t.Fatalf("update %q: %v", text, err)
		}
		if err := s.Finish(ctx, ptrTo(emptyFinal(t))); err != nil {
			t.Fatalf("finish %q: %v", text, err)
		}
	}

	if want := []string{"Let me check.", "Here is the answer."}; !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent = %q, want %q", sent, want)
	}
}

// TestEditMode_EmptyFinalKeepsBufferedTextInPreviewMessage verifies that in
// group chats the preview message is edited to the text it has buffered when
// the preview ends with an empty final, and the next preview opens a message
// of its own instead of overwriting it.
func TestEditMode_EmptyFinalKeepsBufferedTextInPreviewMessage(t *testing.T) {
	ctx := context.Background()
	bot := newTestTelegramBot(telegramRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)),
		}, nil
	}))
	origGetBot := getOrCreateBotForTest
	origSendText := sendTextForTest
	origEdit := testEditFunc
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) { return bot, nil }
	var sent []string
	sendTextForTest = func(_ *tele.Bot, _ string, text string, _ int, _ string) (int64, int, error) {
		sent = append(sent, normalizeStreamComparableText(text))
		return 42, len(sent), nil
	}
	edits := map[int][]string{}
	testEditFunc = func(_ *tele.Bot, _ int64, msgID int, text string, _ string) error {
		edits[msgID] = append(edits[msgID], text)
		return nil
	}
	defer func() {
		getOrCreateBotForTest = origGetBot
		sendTextForTest = origSendText
		testEditFunc = origEdit
	}()

	for _, text := range []string{"Let me check.", "Here is the answer."} {
		s := &telegramPreview{
			adapter: NewTelegramAdapter(nil),
			cfg:     channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
			target:  "123",
		}
		if err := s.Update(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{Type: channel.StreamEventDelta, Delta: text, Phase: channel.StreamPhaseText})); err != nil {
			t.Fatalf("update %q: %v", text, err)
		}
		if err := s.Finish(ctx, ptrTo(emptyFinal(t))); err != nil {
			t.Fatalf("finish %q: %v", text, err)
		}
	}

	if want := []string{"Let me check.", "Here is the answer."}; !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent = %q, want one message per preview %q", sent, want)
	}
	if want := []string{"Let me check."}; !reflect.DeepEqual(edits[1], want) {
		t.Fatalf("edits of the first message = %q, want %q", edits[1], want)
	}
	if want := []string{"Here is the answer."}; !reflect.DeepEqual(edits[2], want) {
		t.Fatalf("edits of the second message = %q, want %q", edits[2], want)
	}
}

func ptrTo[T any](v T) *T { return &v }

// TestStreamFinal_RichPartsUseSendRichMessage verifies that when a
// StreamEventFinal carries canonical Parts and the channel is RichText-capable
// (per item 1), pushFinal routes through sendRichMessage instead of falling
// back to PlainText()+formatTelegramOutput. Without this, rich Parts arriving
// in the streaming-final boundary get silently degraded to markdown text.
func TestStreamFinal_RichPartsUseSendRichMessage(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter: adapter,
		cfg:     channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		target:  "123",
	}
	ctx := context.Background()

	var sawRich bool
	bot := newTestTelegramBot(telegramRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/sendRichMessage") {
			sawRich = true
		}
		resp := `{"ok":true,"result":{"message_id":77,"chat":{"id":123}}}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(resp)),
		}, nil
	}))

	origGetBot := getOrCreateBotForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return bot, nil
	}
	defer func() { getOrCreateBotForTest = origGetBot }()

	err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{
		Type: channel.StreamEventFinal,
		Final: &channel.StreamFinalizePayload{
			Message: channel.Message{
				Format: channel.MessageFormatRich,
				Parts: []channel.MessagePart{
					{Type: channel.MessagePartText, Text: "hello", Styles: []channel.MessageTextStyle{channel.MessageStyleBold}},
				},
			},
		},
	}))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !sawRich {
		t.Fatal("expected sendRichMessage to be invoked for streaming final with Parts")
	}
}

func TestStreamFinal_MarkdownMathUsesRichMessage(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter: adapter,
		cfg:     channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		target:  "123",
	}
	ctx := context.Background()

	var gotBody map[string]any
	bot := newTestTelegramBot(telegramRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(req.URL.Path, "/sendRichMessage") {
			t.Fatalf("expected sendRichMessage, got %s", req.URL.Path)
		}
		body, _ := io.ReadAll(req.Body)
		gotBody = decodeTelegramBody(t, body)
		resp := `{"ok":true,"result":{"message_id":77,"chat":{"id":123}}}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(resp)),
		}, nil
	}))

	origGetBot := getOrCreateBotForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return bot, nil
	}
	defer func() { getOrCreateBotForTest = origGetBot }()

	err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{
		Type: channel.StreamEventFinal,
		Final: &channel.StreamFinalizePayload{
			Message: channel.Message{
				Text:   `Use \[E = mc^2\] here.`,
				Format: channel.MessageFormatMarkdown,
			},
		},
	}))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got, want := telegramRichMarkdownFromBody(t, gotBody), `Use $$E = mc^2$$ here.`; got != want {
		t.Fatalf("unexpected rich markdown:\n got: %q\nwant: %q", got, want)
	}
}

func TestStreamFinal_PlainMathUsesRichMessage(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter: adapter,
		cfg:     channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		target:  "123",
	}
	ctx := context.Background()

	var gotBody map[string]any
	bot := newTestTelegramBot(telegramRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(req.URL.Path, "/sendRichMessage") {
			t.Fatalf("expected sendRichMessage, got %s", req.URL.Path)
		}
		body, _ := io.ReadAll(req.Body)
		gotBody = decodeTelegramBody(t, body)
		resp := `{"ok":true,"result":{"message_id":78,"chat":{"id":123}}}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(resp)),
		}, nil
	}))

	origGetBot := getOrCreateBotForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return bot, nil
	}
	defer func() { getOrCreateBotForTest = origGetBot }()

	text := "麦克斯韦方程组：\n\n$$\n\\nabla \\cdot \\mathbf{E} = \\frac{\\rho}{\\varepsilon_0}\n$$"
	err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{
		Type: channel.StreamEventFinal,
		Final: &channel.StreamFinalizePayload{
			Message: channel.Message{
				Text:   text,
				Format: channel.MessageFormatPlain,
			},
		},
	}))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got := telegramRichMarkdownFromBody(t, gotBody); got != text {
		t.Fatalf("unexpected rich markdown:\n got: %q\nwant: %q", got, text)
	}
}

func TestStreamFinal_MediumRichPartsUseSendRichMessage(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter: adapter,
		cfg:     channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		target:  "123",
	}
	ctx := context.Background()

	var sawRich bool
	bot := newTestTelegramBot(telegramRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/sendRichMessage") {
			sawRich = true
		}
		if strings.HasSuffix(req.URL.Path, "/sendMessage") || strings.HasSuffix(req.URL.Path, "/editMessageText") {
			t.Fatalf("medium rich stream final should use sendRichMessage, got %s", req.URL.Path)
		}
		resp := `{"ok":true,"result":{"message_id":78,"chat":{"id":123}}}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(resp)),
		}, nil
	}))

	origGetBot := getOrCreateBotForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return bot, nil
	}
	defer func() { getOrCreateBotForTest = origGetBot }()

	err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{
		Type: channel.StreamEventFinal,
		Final: &channel.StreamFinalizePayload{
			Message: channel.Message{
				Format: channel.MessageFormatRich,
				Parts: []channel.MessagePart{
					{Type: channel.MessagePartText, Text: strings.Repeat("你", telegramMaxMessageLength+100), Styles: []channel.MessageTextStyle{channel.MessageStyleBold}},
				},
			},
		},
	}))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !sawRich {
		t.Fatal("expected sendRichMessage to be invoked for medium streaming final with Parts")
	}
}

func TestStreamFinal_RichEditUnrecoverableFallsBackToNewRichMessage(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter:      adapter,
		cfg:          channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		target:       "123",
		streamChatID: 123,
		streamMsgID:  77,
	}
	ctx := context.Background()

	var paths []string
	bot := newTestTelegramBot(telegramRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		if strings.HasSuffix(req.URL.Path, "/editMessageText") {
			resp := `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(resp)),
			}, nil
		}
		if !strings.HasSuffix(req.URL.Path, "/sendRichMessage") {
			t.Fatalf("expected sendRichMessage after failed edit, got %s", req.URL.Path)
		}
		resp := `{"ok":true,"result":{"message_id":88,"chat":{"id":123}}}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(resp)),
		}, nil
	}))

	origGetBot := getOrCreateBotForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return bot, nil
	}
	defer func() { getOrCreateBotForTest = origGetBot }()

	err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{
		Type: channel.StreamEventFinal,
		Final: &channel.StreamFinalizePayload{
			Message: channel.Message{
				Format: channel.MessageFormatRich,
				Parts: []channel.MessagePart{
					{Type: channel.MessagePartText, Text: "hello", Styles: []channel.MessageTextStyle{channel.MessageStyleBold}},
				},
			},
		},
	}))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(paths) != 2 ||
		!strings.HasSuffix(paths[0], "/editMessageText") ||
		!strings.HasSuffix(paths[1], "/sendRichMessage") {
		t.Fatalf("expected failed edit followed by new rich send, got paths %v", paths)
	}
}

func TestStreamFinal_RichSendFallbackPreservesParseMode(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter: adapter,
		cfg:     channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		target:  "123",
	}
	ctx := context.Background()

	var sendPayload map[string]any
	var editPayload map[string]any
	bot := newTestTelegramBot(telegramRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		payload := map[string]any{}
		if len(body) > 0 {
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("parse body: %v (raw=%q)", err, string(body))
			}
		}
		if strings.HasSuffix(req.URL.Path, "/sendRichMessage") {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":false,"error_code":404,"description":"Not Found: method not found"}`)),
			}, nil
		}
		if strings.HasSuffix(req.URL.Path, "/sendMessage") {
			sendPayload = payload
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":90,"chat":{"id":123}}}`)),
			}, nil
		}
		if strings.HasSuffix(req.URL.Path, "/editMessageText") {
			editPayload = payload
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":90,"chat":{"id":123}}}`)),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)),
		}, nil
	}))

	origGetBot := getOrCreateBotForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return bot, nil
	}
	defer func() { getOrCreateBotForTest = origGetBot }()

	err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{
		Type: channel.StreamEventFinal,
		Final: &channel.StreamFinalizePayload{
			Message: channel.Message{
				Format: channel.MessageFormatRich,
				Parts: []channel.MessagePart{
					{Type: channel.MessagePartText, Text: "hello", Styles: []channel.MessageTextStyle{channel.MessageStyleBold}},
				},
			},
		},
	}))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got, _ := sendPayload["parse_mode"].(string); got != tele.ModeHTML {
		t.Fatalf("expected fallback send parse_mode=HTML, got payload %v", sendPayload)
	}
	if got, _ := editPayload["parse_mode"].(string); got != tele.ModeHTML {
		t.Fatalf("expected fallback edit parse_mode=HTML, got payload %v", editPayload)
	}
	if got, _ := editPayload["text"].(string); got != "<b>hello</b>" {
		t.Fatalf("expected HTML fallback final body, got %q", got)
	}
}

func TestStreamFinal_RichSendFallbackPreservesActions(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter: adapter,
		cfg:     channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		target:  "123",
	}
	ctx := context.Background()

	var fallbackPayload map[string]any
	bot := newTestTelegramBot(telegramRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		if strings.HasSuffix(req.URL.Path, "/sendChatAction") {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)),
			}, nil
		}
		if strings.HasSuffix(req.URL.Path, "/sendRichMessage") {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"ok":false,"error_code":404,"description":"Not Found: method not found"}`)),
			}, nil
		}
		if !strings.HasSuffix(req.URL.Path, "/sendMessage") {
			t.Fatalf("expected fallback sendMessage, got %s", req.URL.Path)
		}
		fallbackPayload = decodeTelegramBody(t, body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":96,"chat":{"id":123}}}`)),
		}, nil
	}))

	origGetBot := getOrCreateBotForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return bot, nil
	}
	defer func() { getOrCreateBotForTest = origGetBot }()

	err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{
		Type: channel.StreamEventFinal,
		Final: &channel.StreamFinalizePayload{
			Message: channel.Message{
				Format: channel.MessageFormatRich,
				Parts: []channel.MessagePart{
					{Type: channel.MessagePartText, Text: "hello", Styles: []channel.MessageTextStyle{channel.MessageStyleBold}},
				},
				Actions: []channel.Action{{
					Label: "Open",
					URL:   "https://example.com",
				}},
			},
		},
	}))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got, _ := fallbackPayload["text"].(string); got != "<b>hello</b>" {
		t.Fatalf("expected fallback text, got payload %v", fallbackPayload)
	}
	if _, ok := fallbackPayload["reply_markup"]; !ok {
		t.Fatalf("expected fallback reply_markup, got payload %v", fallbackPayload)
	}
}

func TestStreamFinal_LongRichPartsFallbacksToPlainText(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter: adapter,
		cfg:     channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		target:  "123",
	}
	ctx := context.Background()

	var sendPayload map[string]any
	var editPayload map[string]any
	bot := newTestTelegramBot(telegramRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/sendRichMessage") {
			t.Fatalf("long rich stream final should not call sendRichMessage")
		}
		body, _ := io.ReadAll(req.Body)
		payload := map[string]any{}
		if len(body) > 0 {
			payload = decodeTelegramBody(t, body)
		}
		switch {
		case strings.HasSuffix(req.URL.Path, "/sendMessage"):
			sendPayload = payload
		case strings.HasSuffix(req.URL.Path, "/editMessageText"):
			editPayload = payload
		case strings.HasSuffix(req.URL.Path, "/sendChatAction"):
		default:
			t.Fatalf("unexpected Telegram request path %s", req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":91,"chat":{"id":123}}}`)),
		}, nil
	}))

	origGetBot := getOrCreateBotForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return bot, nil
	}
	defer func() { getOrCreateBotForTest = origGetBot }()

	err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{
		Type: channel.StreamEventFinal,
		Final: &channel.StreamFinalizePayload{
			Message: channel.Message{
				Format: channel.MessageFormatRich,
				Parts: []channel.MessagePart{
					{Type: channel.MessagePartText, Text: strings.Repeat("你", telegramMaxMessageLength*9), Styles: []channel.MessageTextStyle{channel.MessageStyleBold}},
				},
			},
		},
	}))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	text, _ := editPayload["text"].(string)
	if utf8.RuneCountInString(text) > telegramMaxMessageLength {
		t.Fatalf("fallback text should be truncated to %d runes, got %d", telegramMaxMessageLength, utf8.RuneCountInString(text))
	}
	if got, _ := sendPayload["parse_mode"].(string); got != "" {
		t.Fatalf("long rich stream initial fallback should use plain text parse mode, got body %v", sendPayload)
	}
	if got, _ := editPayload["parse_mode"].(string); got != "" {
		t.Fatalf("long rich stream final fallback should use plain text parse mode, got body %v", editPayload)
	}
}

func TestStreamFinal_RichHTMLOverflowFallbackSplitsPlainText(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter:       adapter,
		cfg:           channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		target:        "123",
		isPrivateChat: true,
	}
	ctx := context.Background()

	var bodies []map[string]any
	bot := newTestTelegramBot(telegramRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/sendRichMessage") {
			t.Fatalf("HTML-overflow rich stream final should not call sendRichMessage")
		}
		if !strings.HasSuffix(req.URL.Path, "/sendMessage") {
			t.Fatalf("expected sendMessage, got %s", req.URL.Path)
		}
		body, _ := io.ReadAll(req.Body)
		bodies = append(bodies, decodeTelegramBody(t, body))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":92,"chat":{"id":123}}}`)),
		}, nil
	}))

	origGetBot := getOrCreateBotForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return bot, nil
	}
	defer func() { getOrCreateBotForTest = origGetBot }()

	err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{
		Type: channel.StreamEventFinal,
		Final: &channel.StreamFinalizePayload{
			Message: channel.Message{
				Format: channel.MessageFormatRich,
				Parts:  telegramHTMLOverflowParts(),
			},
		},
	}))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(bodies) < 2 {
		t.Fatalf("expected fallback plain text to split, got %d sendMessage call(s)", len(bodies))
	}
	var joined strings.Builder
	for i, body := range bodies {
		text, _ := body["text"].(string)
		if got := utf8.RuneCountInString(text); got > telegramMaxMessageLength {
			t.Fatalf("fallback chunk %d exceeds Telegram text limit: %d", i, got)
		}
		joined.WriteString(text)
	}
	if got := utf8.RuneCountInString(joined.String()); got <= telegramMaxMessageLength {
		t.Fatalf("fallback sent only one truncated text window, got %d runes", got)
	}
}

func TestStreamFinal_RichHTMLOverflowRejectsInvalidActionsBeforeFallbackChunks(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter:       adapter,
		cfg:           channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		target:        "123",
		isPrivateChat: true,
	}

	bot := newTestTelegramBot(telegramRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("invalid rich fallback action should fail before HTTP request: %s", req.URL.Path)
		return nil, nil
	}))

	origGetBot := getOrCreateBotForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return bot, nil
	}
	defer func() { getOrCreateBotForTest = origGetBot }()

	err := s.drive(context.Background(), mustPreparedTelegramEvent(t, channel.StreamEvent{
		Type: channel.StreamEventFinal,
		Final: &channel.StreamFinalizePayload{
			Message: channel.Message{
				Format: channel.MessageFormatRich,
				Parts:  telegramHTMLOverflowParts(),
				Actions: []channel.Action{{
					Label: "Too large",
					Value: strings.Repeat("x", telegramMaxCallbackDataBytes+1),
				}},
			},
		},
	}))
	if err == nil || !strings.Contains(err.Error(), "callback data") {
		t.Fatalf("expected invalid action error before fallback chunks, got %v", err)
	}
}

func TestStreamFinal_RichHTMLOverflowFallbackClearsParseModeOnExistingMessage(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter:      adapter,
		cfg:          channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		target:       "123",
		parseMode:    tele.ModeHTML,
		streamChatID: 123,
		streamMsgID:  77,
		lastEdited:   "pending",
	}
	ctx := context.Background()

	var editParseMode string
	var sendBodies []map[string]any
	origEdit := testEditFunc
	testEditFunc = func(_ *tele.Bot, _ int64, _ int, _ string, parseMode string) error {
		editParseMode = parseMode
		return nil
	}
	defer func() { testEditFunc = origEdit }()

	bot := newTestTelegramBot(telegramRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/sendRichMessage") {
			t.Fatalf("HTML-overflow rich stream final should not call sendRichMessage")
		}
		if !strings.HasSuffix(req.URL.Path, "/sendMessage") {
			t.Fatalf("expected sendMessage, got %s", req.URL.Path)
		}
		body, _ := io.ReadAll(req.Body)
		sendBodies = append(sendBodies, decodeTelegramBody(t, body))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":93,"chat":{"id":123}}}`)),
		}, nil
	}))

	origGetBot := getOrCreateBotForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return bot, nil
	}
	defer func() { getOrCreateBotForTest = origGetBot }()

	err := s.drive(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{
		Type: channel.StreamEventFinal,
		Final: &channel.StreamFinalizePayload{
			Message: channel.Message{
				Format: channel.MessageFormatRich,
				Parts:  telegramHTMLOverflowParts(),
			},
		},
	}))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if editParseMode != "" {
		t.Fatalf("existing stream message fallback edit should clear parse mode, got %q", editParseMode)
	}
	if len(sendBodies) == 0 {
		t.Fatal("expected remaining fallback chunks to be sent")
	}
	for i, body := range sendBodies {
		if got, _ := body["parse_mode"].(string); got != "" {
			t.Fatalf("fallback send chunk %d should use plain parse mode, got %q", i, got)
		}
	}
}

func TestStreamFinal_AttachmentOnlyWithActionsDoesNotSendPlaceholderText(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter: adapter,
		cfg: channel.ChannelConfig{
			ID:          "test",
			BotID:       "bot-test",
			ChannelType: channel.ChannelTypeTelegram,
			Credentials: map[string]any{"bot_token": "fake"},
		},
		target: "123",
	}

	var paths []string
	var photoBody string
	bot := newTestTelegramBot(telegramRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		body, _ := io.ReadAll(req.Body)
		if strings.HasSuffix(req.URL.Path, "/sendMessage") {
			t.Fatalf("attachment-only final should not send placeholder text: %s", body)
		}
		if strings.HasSuffix(req.URL.Path, "/sendPhoto") {
			photoBody = string(body)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":94,"chat":{"id":123},"photo":[{"file_id":"photo-1","file_unique_id":"u1","width":1,"height":1}]}}`)),
		}, nil
	}))

	origGetBot := getOrCreateBotForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return bot, nil
	}
	defer func() { getOrCreateBotForTest = origGetBot }()

	err := s.drive(context.Background(), mustPreparedTelegramEvent(t, channel.StreamEvent{
		Type: channel.StreamEventFinal,
		Final: &channel.StreamFinalizePayload{
			Message: channel.Message{
				Attachments: []channel.Attachment{{
					Type: channel.AttachmentImage,
					URL:  "https://example.com/photo.jpg",
				}},
				Actions: []channel.Action{{
					Label: "Open",
					URL:   "https://example.com",
				}},
			},
		},
	}))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(paths) != 1 || !strings.HasSuffix(paths[0], "/sendPhoto") {
		t.Fatalf("expected only sendPhoto, got %v", paths)
	}
	if !strings.Contains(photoBody, "reply_markup") || !strings.Contains(photoBody, "https://example.com") {
		t.Fatalf("expected attachment request to carry inline keyboard, body=%s", photoBody)
	}
}

func TestStreamFinal_AttachmentActionsRejectInvalidBeforeTextOrUpload(t *testing.T) {
	adapter := NewTelegramAdapter(nil)
	s := &telegramPreview{
		adapter: adapter,
		cfg: channel.ChannelConfig{
			ID:          "test",
			BotID:       "bot-test",
			ChannelType: channel.ChannelTypeTelegram,
			Credentials: map[string]any{"bot_token": "fake"},
		},
		target: "123",
	}

	bot := newTestTelegramBot(telegramRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("invalid attachment action should fail before HTTP request: %s", req.URL.Path)
		return nil, nil
	}))

	origGetBot := getOrCreateBotForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) {
		return bot, nil
	}
	defer func() { getOrCreateBotForTest = origGetBot }()

	err := s.drive(context.Background(), mustPreparedTelegramEvent(t, channel.StreamEvent{
		Type: channel.StreamEventFinal,
		Final: &channel.StreamFinalizePayload{
			Message: channel.Message{
				Text: "Ready",
				Attachments: []channel.Attachment{{
					Type: channel.AttachmentImage,
					URL:  "https://example.com/photo.jpg",
				}},
				Actions: []channel.Action{{
					Label: "Too large",
					Value: strings.Repeat("x", telegramMaxCallbackDataBytes+1),
				}},
			},
		},
	}))
	if err == nil || !strings.Contains(err.Error(), "callback data") {
		t.Fatalf("expected invalid attachment action error before send, got %v", err)
	}
}

func TestDescriptorDeclaresDraftPreview(t *testing.T) {
	t.Parallel()

	adapter := NewTelegramAdapter(nil)
	caps := adapter.Descriptor().Capabilities
	if caps.Preview != channel.PreviewDraft {
		t.Fatalf("Preview = %v, want PreviewDraft", caps.Preview)
	}
	if caps.ReplyWindow != 0 {
		t.Fatalf("ReplyWindow = %v, the Bot API sets no window on replying to a message", caps.ReplyWindow)
	}
}

// A private chat streams as a draft, any other chat edits one message.
func TestOpenPreviewChoosesTheModeByChatType(t *testing.T) {
	t.Parallel()

	cfg := channel.ChannelConfig{ID: "cfg-1", Credentials: map[string]any{"botToken": "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ"}}
	cases := []struct {
		name        string
		target      string
		opts        channel.StreamOptions
		wantPrivate bool
	}{
		{"private chat", "12345", channel.StreamOptions{Metadata: map[string]any{"conversation_type": "private"}}, true},
		{"group chat", "-100123", channel.StreamOptions{Metadata: map[string]any{"conversation_type": "group"}}, false},
		{"channel username", "@news", channel.StreamOptions{Metadata: map[string]any{"conversation_type": "private"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			preview, err := NewTelegramAdapter(nil).OpenPreview(context.Background(), cfg, tc.target, tc.opts)
			if err != nil {
				t.Fatalf("OpenPreview: %v", err)
			}
			got, ok := preview.(*telegramPreview)
			if !ok {
				t.Fatalf("preview is %T, want *telegramPreview", preview)
			}
			if got.isPrivateChat != tc.wantPrivate {
				t.Fatalf("isPrivateChat = %v, want %v", got.isPrivateChat, tc.wantPrivate)
			}
		})
	}
}

// recordTelegramCalls answers every Bot API call with success and records the
// method and the parameters it was called with.
func recordTelegramCalls(t *testing.T) (*tele.Bot, func() []string) {
	t.Helper()
	var calls []string
	var mu sync.Mutex
	bot := newTestTelegramBot(telegramRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		params := decodeTelegramBody(t, body)
		mu.Lock()
		calls = append(calls, req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]+" "+fmt.Sprint(params["chat_id"])+" "+fmt.Sprint(params["message_id"]))
		mu.Unlock()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)),
		}, nil
	}))
	return bot, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string{}, calls...)
	}
}

// A reply that turns out to be empty must not leave the preview message with
// its pending marker behind. A message the preview already completed, and a
// draft, stay as they are.
func TestPreview_FinishWithNoFinalRemovesOnlyAPendingMessage(t *testing.T) {
	cases := []struct {
		name  string
		setup func(s *telegramPreview)
		want  []string
	}{
		{
			name: "pending message",
			setup: func(s *telegramPreview) {
				s.streamChatID, s.streamMsgID, s.lastEdited = 42, 7, "NO_REPLY"+telegramStreamPendingSuffix
			},
			want: []string{"deleteMessage 42 7"},
		},
		{
			name: "completed message",
			setup: func(s *telegramPreview) {
				s.streamChatID, s.streamMsgID, s.lastEdited = 42, 7, "done"
			},
		},
		{
			name:  "no message yet",
			setup: func(*telegramPreview) {},
		},
		{
			name: "draft",
			setup: func(s *telegramPreview) {
				s.isPrivateChat = true
				s.streamChatID, s.streamMsgID, s.lastEdited = 42, 7, "text"+telegramStreamPendingSuffix
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bot, calls := recordTelegramCalls(t)
			origGetBot := getOrCreateBotForTest
			getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) { return bot, nil }
			defer func() { getOrCreateBotForTest = origGetBot }()

			s := &telegramPreview{
				adapter: NewTelegramAdapter(nil),
				cfg:     channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
			}
			tc.setup(s)
			if err := s.Finish(context.Background(), nil); err != nil {
				t.Fatalf("Finish(nil): %v", err)
			}
			if got := calls(); strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("API calls = %q, want %q", got, tc.want)
			}
		})
	}
}

// An error is the reply's terminal state: it takes the place of the text a
// reset left in the preview message, and the final that may follow does not
// edit it away.
func TestPreview_ErrorReplacesResetTextAndIsNotEditedAway(t *testing.T) {
	bot, _ := recordTelegramCalls(t)
	origGetBot := getOrCreateBotForTest
	origSendText := sendTextForTest
	origEdit := testEditFunc
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) { return bot, nil }
	var sends []string
	sendTextForTest = func(_ *tele.Bot, _ string, text string, _ int, _ string) (int64, int, error) {
		sends = append(sends, text)
		return 42, 8, nil
	}
	type edit struct {
		msgID int
		text  string
	}
	var edits []edit
	testEditFunc = func(_ *tele.Bot, _ int64, msgID int, text string, _ string) error {
		edits = append(edits, edit{msgID, text})
		return nil
	}
	defer func() {
		getOrCreateBotForTest = origGetBot
		sendTextForTest = origSendText
		testEditFunc = origEdit
	}()

	s := &telegramPreview{
		adapter:      NewTelegramAdapter(nil),
		cfg:          channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		target:       "42",
		streamChatID: 42,
		streamMsgID:  7,
		lastEdited:   "failed attempt" + telegramStreamPendingSuffix,
	}
	ctx := context.Background()
	if err := s.Update(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{Type: channel.StreamEventError, Error: "Something went wrong.", ErrorCode: "internal"})); err != nil {
		t.Fatalf("Update(error): %v", err)
	}
	final := mustPreparedTelegramEvent(t, channel.StreamEvent{
		Type:  channel.StreamEventFinal,
		Final: &channel.StreamFinalizePayload{Message: channel.Message{Text: "answer"}},
	})
	if err := s.Finish(ctx, &final); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	want := []edit{{7, "Something went wrong."}, {8, "answer"}}
	if !reflect.DeepEqual(edits, want) {
		t.Fatalf("edits = %+v, want %+v", edits, want)
	}
	if len(sends) != 1 {
		t.Fatalf("sends = %q, want the final posted as one new message", sends)
	}
}

// In a private chat the error is a message of its own and does not count as
// the reply's text, so a final after it is still posted.
func TestPreview_PrivateErrorDoesNotSwallowTheFinal(t *testing.T) {
	origGetBot := getOrCreateBotForTest
	origSendText := sendTextForTest
	getOrCreateBotForTest = func(_ *TelegramAdapter, _, _ string) (*tele.Bot, error) { return &tele.Bot{Token: "fake"}, nil }
	var sent []string
	sendTextForTest = func(_ *tele.Bot, _ string, text string, _ int, _ string) (int64, int, error) {
		sent = append(sent, text)
		return 123, 1, nil
	}
	defer func() {
		getOrCreateBotForTest = origGetBot
		sendTextForTest = origSendText
	}()

	s := &telegramPreview{
		adapter:       NewTelegramAdapter(nil),
		cfg:           channel.ChannelConfig{ID: "test", Credentials: map[string]any{"bot_token": "fake"}},
		target:        "123",
		isPrivateChat: true,
		draftID:       1,
		streamChatID:  123,
	}
	ctx := context.Background()
	if err := s.Update(ctx, mustPreparedTelegramEvent(t, channel.StreamEvent{Type: channel.StreamEventError, Error: "Something went wrong.", ErrorCode: "internal"})); err != nil {
		t.Fatalf("Update(error): %v", err)
	}
	final := mustPreparedTelegramEvent(t, channel.StreamEvent{
		Type:  channel.StreamEventFinal,
		Final: &channel.StreamFinalizePayload{Message: channel.Message{Text: "answer"}},
	})
	if err := s.Finish(ctx, &final); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if want := []string{"Something went wrong.", "answer"}; !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent = %q, want %q", sent, want)
	}
}
