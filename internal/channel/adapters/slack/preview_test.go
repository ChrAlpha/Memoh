package slack

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/felinics/memoh/internal/channel"
)

type slackCall struct {
	Method string
	Form   url.Values
}

func (c slackCall) text() string { return c.Form.Get("text") }

// slackAPI stands in for the Slack Web API: it records every call and answers
// a post with a new message timestamp.
type slackAPI struct {
	mu     sync.Mutex
	calls  []slackCall
	posted int
}

func (a *slackAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	const apiURL = "https://slack.test/api/"
	if strings.HasPrefix(r.URL.String(), "https://upload.slack.test/") {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
	}
	if !strings.HasPrefix(r.URL.String(), apiURL) {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("not found")), Header: make(http.Header)}, nil
	}
	method := strings.TrimPrefix(r.URL.String(), apiURL)
	_ = r.ParseForm()

	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, slackCall{Method: method, Form: r.PostForm})
	reply := `{"ok":true,"channel":"C123","ts":"1710000000.000100"}`
	switch method {
	case "chat.postMessage":
		a.posted++
		reply = fmt.Sprintf(`{"ok":true,"channel":"C123","ts":"1710000000.00010%d"}`, a.posted)
	case "files.getUploadURLExternal":
		reply = `{"ok":true,"upload_url":"https://upload.slack.test/F200","file_id":"F200"}`
	case "files.completeUploadExternal":
		reply = `{"ok":true,"files":[{"id":"F200"}]}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(reply)),
	}, nil
}

func (a *slackAPI) snapshot() []slackCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]slackCall{}, a.calls...)
}

// callAt is the i-th recorded call; the test stops when there is none.
func (a *slackAPI) callAt(t *testing.T, i int) slackCall {
	t.Helper()
	calls := a.snapshot()
	if i >= len(calls) {
		t.Fatalf("call %d missing, calls = %v", i, a.summary())
	}
	return calls[i]
}

// summary lists the API methods called, in order.
func (a *slackAPI) summary() []string {
	calls := a.snapshot()
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Method)
	}
	return out
}

func newTestPreview(t *testing.T, reply *channel.ReplyRef) (*slackPreview, *slackAPI) {
	t.Helper()
	api := &slackAPI{}
	client := slack.New(
		testBotToken,
		slack.OptionAPIURL("https://slack.test/api/"),
		slack.OptionHTTPClient(&http.Client{Transport: api}),
	)
	return &slackPreview{
		adapter: NewSlackAdapter(nil),
		cfg:     channel.ChannelConfig{ID: "cfg-preview"},
		target:  "C123",
		reply:   reply,
		api:     client,
	}, api
}

func started() channel.PreparedStreamEvent {
	return channel.PreparedStreamEvent{Type: channel.StreamEventStatus, Status: channel.StreamStatusStarted}
}

func textDelta(text string) channel.PreparedStreamEvent {
	return channel.PreparedStreamEvent{Type: channel.StreamEventDelta, Delta: text, Phase: channel.StreamPhaseText}
}

func finalOf(msg channel.Message, attachments ...channel.PreparedAttachment) *channel.PreparedStreamEvent {
	return &channel.PreparedStreamEvent{
		Type: channel.StreamEventFinal,
		Final: &channel.PreparedStreamFinalizePayload{
			Message: channel.PreparedMessage{Message: msg, Attachments: attachments},
		},
	}
}

func mustUpdate(t *testing.T, p *slackPreview, event channel.PreparedStreamEvent) {
	t.Helper()
	if err := p.Update(context.Background(), event); err != nil {
		t.Fatalf("update %s: %v", event.Type, err)
	}
}

func mustFinish(t *testing.T, p *slackPreview, final *channel.PreparedStreamEvent) {
	t.Helper()
	if err := p.Finish(context.Background(), final); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func equalStrings(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// allowUpdate lifts the throttle that spaces the updates of streamed text.
func allowUpdate(p *slackPreview) {
	p.mu.Lock()
	p.nextUpdate = time.Now().Add(-time.Second)
	p.mu.Unlock()
}

func TestSlackPreviewCapabilities(t *testing.T) {
	t.Parallel()

	var _ channel.PreviewOpener = (*SlackAdapter)(nil)
	caps := NewSlackAdapter(nil).Descriptor().Capabilities
	if caps.Preview != channel.PreviewEditInPlace {
		t.Fatalf("preview mode = %v, want edit in place", caps.Preview)
	}
	if caps.NativeApproval {
		t.Fatal("Slack renders no approval request itself; the delivery layer sends the text prompt")
	}
}

func TestSlackOpenPreviewResolvesUserTargetToDMChannel(t *testing.T) {
	t.Parallel()

	adapter := NewSlackAdapter(nil)
	adapter.openConversation = func(_ context.Context, _ *slack.Client, params *slack.OpenConversationParameters) (*slack.Channel, bool, bool, error) {
		if len(params.Users) != 1 || params.Users[0] != "U123" {
			t.Fatalf("unexpected users: %#v", params.Users)
		}
		return &slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "D999"}}}, false, false, nil
	}

	preview, err := adapter.OpenPreview(context.Background(), channel.ChannelConfig{
		Credentials: map[string]any{
			"botToken": testShortBotToken,
			"appToken": testAppToken,
		},
	}, "U123", channel.StreamOptions{SourceMessageID: "1710000000.000050"})
	if err != nil {
		t.Fatalf("OpenPreview: %v", err)
	}

	opened, ok := preview.(*slackPreview)
	if !ok {
		t.Fatalf("unexpected preview type %T", preview)
	}
	if opened.target != "D999" {
		t.Fatalf("expected resolved DM target, got %q", opened.target)
	}
	if opened.reply == nil || opened.reply.MessageID != "1710000000.000050" {
		t.Fatalf("reply = %+v, want the source message as the thread", opened.reply)
	}
}

// The preview opens as a placeholder in the thread of the message it answers,
// and the final text replaces the placeholder in that same message.
func TestSlackPreviewFinishUpdatesThePlaceholderInPlace(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, &channel.ReplyRef{MessageID: "1710000000.000050"})
	mustUpdate(t, p, started())
	mustUpdate(t, p, textDelta("hello "))
	mustFinish(t, p, finalOf(channel.Message{Text: "hello world"}))

	if got := api.summary(); !equalStrings(got, "chat.postMessage", "chat.update") {
		t.Fatalf("calls = %v", got)
	}
	calls := api.snapshot()
	if calls[0].text() != slackThinkingText || calls[0].Form.Get("thread_ts") != "1710000000.000050" {
		t.Fatalf("placeholder = %v", calls[0].Form)
	}
	if calls[1].text() != "hello world" || calls[1].Form.Get("ts") != "1710000000.000101" {
		t.Fatalf("final update = %v", calls[1].Form)
	}
}

func TestSlackPreviewFinishWithoutPlaceholderPostsTheMessage(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil)
	mustFinish(t, p, finalOf(channel.Message{Text: "answer"}))

	calls := api.snapshot()
	if len(calls) != 1 || calls[0].Method != "chat.postMessage" || calls[0].text() != "answer" {
		t.Fatalf("calls = %v", api.summary())
	}
}

// Streamed text is shown by updating the open message, at most once per
// throttle interval.
func TestSlackPreviewUpdatesStreamedTextWhenDue(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil)
	mustUpdate(t, p, started())
	mustUpdate(t, p, textDelta("first "))
	if got := api.summary(); len(got) != 1 {
		t.Fatalf("an update inside the throttle: %v", got)
	}
	allowUpdate(p)
	mustUpdate(t, p, textDelta("second"))

	calls := api.snapshot()
	if len(calls) != 2 || calls[1].Method != "chat.update" || calls[1].text() != "first second" {
		t.Fatalf("calls = %v", api.summary())
	}
}

// Text beyond the platform's size is cut to it; splitting a long reply across
// messages is the manager's work.
func TestSlackPreviewCutsTextToThePlatformSize(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil)
	mustFinish(t, p, finalOf(channel.Message{Text: strings.Repeat("a", slackMaxLength+500)}))

	got := api.callAt(t, 0).text()
	if len([]rune(got)) != slackMaxLength || !strings.HasSuffix(got, "...") {
		t.Fatalf("sent %d runes", len([]rune(got)))
	}
}

func TestSlackPreviewFinishUploadsAttachmentsInTheThread(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, &channel.ReplyRef{MessageID: "1710000000.000050"})
	mustUpdate(t, p, started())
	mustFinish(t, p, finalOf(channel.Message{Text: "see file"}, preparedSlackUploadAttachment("hello.txt", "text/plain", "hello")))

	if got := api.summary(); !equalStrings(got, "chat.postMessage", "chat.update", "files.getUploadURLExternal", "files.completeUploadExternal") {
		t.Fatalf("calls = %v", got)
	}
}

// A final that carries only attachments leaves no placeholder behind.
func TestSlackPreviewFinishClearsThePlaceholderOfAnAttachmentOnlyFinal(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil)
	mustUpdate(t, p, started())
	mustFinish(t, p, finalOf(channel.Message{}, preparedSlackUploadAttachment("hello.txt", "text/plain", "hello")))

	if got := api.summary(); !equalStrings(got, "chat.postMessage", "chat.delete", "files.getUploadURLExternal", "files.completeUploadExternal") {
		t.Fatalf("calls = %v, want the placeholder deleted before the upload", got)
	}
	if p.msgTS != "" {
		t.Fatalf("placeholder state not cleared, msgTS=%q", p.msgTS)
	}
}

// When the final cannot update the open message, it is posted as a new one and
// the old message goes.
func TestSlackPreviewFinishFallbackDeletesOldPlaceholder(t *testing.T) {
	t.Parallel()

	var postCalls, updateCalls, deleteCalls int
	client := slack.New(
		testBotToken,
		slack.OptionAPIURL("https://slack.test/api/"),
		slack.OptionHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			respond := func(body string) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			}
			switch r.URL.String() {
			case "https://slack.test/api/chat.postMessage":
				postCalls++
				return respond(fmt.Sprintf(`{"ok":true,"channel":"C123","ts":"1710000000.00040%d"}`, postCalls))
			case "https://slack.test/api/chat.update":
				updateCalls++
				return respond(`{"ok":false,"error":"cant_update_message"}`)
			case "https://slack.test/api/chat.delete":
				deleteCalls++
				return respond(`{"ok":true,"channel":"C123","ts":"1710000000.000401"}`)
			default:
				return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("not found")), Header: make(http.Header)}, nil
			}
		})}),
		slack.OptionRetry(3),
	)
	p := &slackPreview{adapter: NewSlackAdapter(nil), cfg: channel.ChannelConfig{ID: "cfg-fallback"}, target: "C123", api: client}
	mustUpdate(t, p, started())
	mustFinish(t, p, finalOf(channel.Message{Text: "final answer"}))

	if postCalls != 2 || updateCalls != 1 || deleteCalls != 1 {
		t.Fatalf("post/update/delete calls = %d/%d/%d, want 2/1/1", postCalls, updateCalls, deleteCalls)
	}
	if p.msgTS != "1710000000.000402" {
		t.Fatalf("expected the preview to track the replacement message, got %q", p.msgTS)
	}
}

func TestSlackPreviewFinishUsesPartsRenderer(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil)
	mustFinish(t, p, finalOf(channel.Message{
		Text:   "plain fallback",
		Format: channel.MessageFormatRich,
		Parts: []channel.MessagePart{
			{Type: channel.MessagePartText, Text: "Hello", Styles: []channel.MessageTextStyle{channel.MessageStyleBold}},
			{Type: channel.MessagePartLink, Text: "docs", URL: "https://example.test/a"},
		},
	}))

	want := "*Hello*\n\n<https://example.test/a|docs>"
	if got := api.callAt(t, 0).text(); got != want {
		t.Fatalf("Slack preview rich text mismatch\n  got:  %q\n  want: %q", got, want)
	}
}

func TestSlackPreviewFinishPreservesURLActionsOnUpdate(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil)
	mustUpdate(t, p, started())
	mustFinish(t, p, finalOf(channel.Message{
		Text:    "Read this",
		Actions: []channel.Action{{Label: "Open docs", URL: "https://example.test/docs"}},
	}))

	var blocks string
	for _, c := range api.snapshot() {
		if c.Method == "chat.update" {
			blocks = c.Form.Get("blocks")
		}
	}
	if !strings.Contains(blocks, `"type":"button"`) ||
		!strings.Contains(blocks, `"text":"Open docs"`) ||
		!strings.Contains(blocks, `"url":"https://example.test/docs"`) {
		t.Fatalf("expected Slack final update button blocks, got %s", blocks)
	}
}

// An error shown before any text is the reply's end: it takes the place of
// the placeholder, and nothing removes it afterwards.
func TestSlackPreviewErrorReplacesThePlaceholder(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil)
	mustUpdate(t, p, started())
	mustUpdate(t, p, channel.PreparedStreamEvent{Type: channel.StreamEventError, Error: "The workspace is unreachable.", ErrorCode: "workspace.unreachable"})
	mustFinish(t, p, nil)

	calls := api.snapshot()
	if len(calls) != 2 || calls[1].Method != "chat.update" || calls[1].text() != "The workspace is unreachable." {
		t.Fatalf("calls = %v", api.summary())
	}
}

func TestSlackPreviewErrorReply(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		event channel.PreparedStreamEvent
		want  []string
	}{
		{
			name:  "coded error shows the copy as it is",
			event: channel.PreparedStreamEvent{Type: channel.StreamEventError, Error: "The workspace is unreachable.", ErrorCode: "workspace.unreachable"},
			want:  []string{"The workspace is unreachable."},
		},
		{
			name:  "uncoded error is labelled",
			event: channel.PreparedStreamEvent{Type: channel.StreamEventError, Error: "request failed"},
			want:  []string{"Error: request failed"},
		},
		{
			name:  "blank error sends nothing",
			event: channel.PreparedStreamEvent{Type: channel.StreamEventError, Error: "  "},
			want:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, api := newTestPreview(t, nil)
			mustUpdate(t, p, tc.event)
			var sent []string
			for _, c := range api.snapshot() {
				if c.Method != "chat.postMessage" {
					t.Fatalf("unexpected call %s", c.Method)
				}
				sent = append(sent, c.text())
			}
			if !equalStrings(sent, tc.want...) {
				t.Fatalf("sent messages = %q, want %q", sent, tc.want)
			}
		})
	}
}

// Text a tool call interrupts is kept as its own message, and the tool call is
// one message that its end updates. A placeholder that no text took is removed
// before the tool call is shown.
func TestSlackPreviewShowsToolCallsInOneMessage(t *testing.T) {
	t.Parallel()

	call := &channel.StreamToolCall{Name: "read", CallID: "c1", Input: map[string]any{"path": "/tmp/a"}}
	done := *call
	done.Result = map[string]any{"ok": true}

	t.Run("after text", func(t *testing.T) {
		t.Parallel()
		p, api := newTestPreview(t, nil)
		mustUpdate(t, p, started())
		mustUpdate(t, p, textDelta("checking"))
		mustUpdate(t, p, channel.PreparedStreamEvent{Type: channel.StreamEventToolCallStart, ToolCall: call})
		mustUpdate(t, p, channel.PreparedStreamEvent{Type: channel.StreamEventToolCallEnd, ToolCall: &done})

		want := []string{"chat.postMessage", "chat.update", "chat.postMessage", "chat.update"}
		if got := api.summary(); !equalStrings(got, want...) {
			t.Fatalf("calls = %v, want %v", got, want)
		}
		if got := api.callAt(t, 1).text(); got != "checking" {
			t.Fatalf("text before the tool = %q", got)
		}
	})
	t.Run("without text", func(t *testing.T) {
		t.Parallel()
		p, api := newTestPreview(t, nil)
		mustUpdate(t, p, started())
		mustUpdate(t, p, channel.PreparedStreamEvent{Type: channel.StreamEventToolCallStart, ToolCall: call})
		mustUpdate(t, p, channel.PreparedStreamEvent{Type: channel.StreamEventToolCallEnd, ToolCall: &done})

		want := []string{"chat.postMessage", "chat.delete", "chat.postMessage", "chat.update"}
		if got := api.summary(); !equalStrings(got, want...) {
			t.Fatalf("calls = %v, want %v", got, want)
		}
	})
}

// A final with no message of its own leaves the streamed text as the reply,
// including text the throttle held back.
func TestSlackPreviewEmptyFinalKeepsBufferedText(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil)
	mustUpdate(t, p, started())
	mustUpdate(t, p, textDelta("held back"))
	mustFinish(t, p, finalOf(channel.Message{}))

	calls := api.snapshot()
	if len(calls) != 2 || calls[1].Method != "chat.update" || calls[1].text() != "held back" {
		t.Fatalf("calls = %v", api.summary())
	}
}

// A nil final means the run said nothing: the placeholder goes. Text or an
// error already shown stays.
func TestSlackPreviewFinishWithoutFinalRemovesOnlyThePlaceholder(t *testing.T) {
	t.Parallel()

	t.Run("placeholder is removed", func(t *testing.T) {
		t.Parallel()
		p, api := newTestPreview(t, nil)
		mustUpdate(t, p, started())
		mustFinish(t, p, nil)
		if got := api.summary(); !equalStrings(got, "chat.postMessage", "chat.delete") {
			t.Fatalf("calls = %v", got)
		}
	})
	t.Run("nothing open sends nothing", func(t *testing.T) {
		t.Parallel()
		p, api := newTestPreview(t, nil)
		mustFinish(t, p, nil)
		if got := api.summary(); len(got) != 0 {
			t.Fatalf("calls = %v", got)
		}
	})
	t.Run("streamed text stays", func(t *testing.T) {
		t.Parallel()
		p, api := newTestPreview(t, nil)
		mustUpdate(t, p, started())
		allowUpdate(p)
		mustUpdate(t, p, textDelta("draft"))
		mustFinish(t, p, nil)
		if got := api.summary(); !equalStrings(got, "chat.postMessage", "chat.update") {
			t.Fatalf("calls = %v", got)
		}
	})
}

func TestSlackPreviewFinishTakesEffectOnce(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil)
	mustUpdate(t, p, started())
	mustFinish(t, p, finalOf(channel.Message{Text: "answer"}))
	mustFinish(t, p, finalOf(channel.Message{Text: "again"}))
	mustFinish(t, p, nil)
	mustUpdate(t, p, textDelta("late"))

	if got := api.summary(); !equalStrings(got, "chat.postMessage", "chat.update") {
		t.Fatalf("calls = %v, want one placeholder and one update", got)
	}
}

// Abort adds nothing, and nothing is shown after it.
func TestSlackPreviewAbortShowsNothingMore(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil)
	mustUpdate(t, p, started())
	if err := p.Abort(context.Background()); err != nil {
		t.Fatalf("abort: %v", err)
	}
	mustUpdate(t, p, textDelta("late"))
	mustFinish(t, p, finalOf(channel.Message{Text: "late"}))

	if got := api.summary(); !equalStrings(got, "chat.postMessage") {
		t.Fatalf("calls = %v, want the placeholder only", got)
	}
}
