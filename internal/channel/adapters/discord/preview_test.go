package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/felinics/memoh/internal/channel"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type discordCall struct {
	Method      string
	Path        string
	ContentType string
	Body        string
}

// content is the message text the call carries.
func (c discordCall) content() string {
	var payload struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal([]byte(c.Body), &payload)
	return payload.Content
}

func (c discordCall) payload(t *testing.T) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(c.Body), &payload); err != nil {
		t.Fatalf("decode %s %s body: %v (body=%q)", c.Method, c.Path, err, c.Body)
	}
	return payload
}

// discordAPI stands in for the Discord REST API: it records every call and
// answers a send with a new message id.
type discordAPI struct {
	mu    sync.Mutex
	calls []discordCall
	sent  int
}

func (a *discordAPI) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, discordCall{
		Method:      req.Method,
		Path:        req.URL.Path,
		ContentType: req.Header.Get("Content-Type"),
		Body:        string(body),
	})
	status, reply := http.StatusOK, `{"id":"msg-edited","channel_id":"ch-1"}`
	switch req.Method {
	case http.MethodPost:
		a.sent++
		reply = fmt.Sprintf(`{"id":"msg-%d","channel_id":"ch-1"}`, a.sent)
	case http.MethodDelete:
		status, reply = http.StatusNoContent, ""
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(reply)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

func (a *discordAPI) snapshot() []discordCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]discordCall{}, a.calls...)
}

// callAt is the i-th recorded call; the test stops when there is none.
func (a *discordAPI) callAt(t *testing.T, i int) discordCall {
	t.Helper()
	calls := a.snapshot()
	if i >= len(calls) {
		t.Fatalf("call %d missing, calls = %v", i, a.summary())
	}
	return calls[i]
}

// summary lists the calls as "METHOD path", the shape most assertions need.
func (a *discordAPI) summary() []string {
	calls := a.snapshot()
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Method+" "+strings.TrimPrefix(c.Path, "/api/v9"))
	}
	return out
}

func newTestPreview(t *testing.T, reply *channel.ReplyRef) (*discordPreview, *discordAPI) {
	t.Helper()
	api := &discordAPI{}
	session, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	session.Client = &http.Client{Transport: api}
	return &discordPreview{target: "ch-1", reply: reply, session: session}, api
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

func mustUpdate(t *testing.T, p *discordPreview, event channel.PreparedStreamEvent) {
	t.Helper()
	if err := p.Update(context.Background(), event); err != nil {
		t.Fatalf("update %s: %v", event.Type, err)
	}
}

func mustFinish(t *testing.T, p *discordPreview, final *channel.PreparedStreamEvent) {
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

func TestDiscordPreviewIsAPreparedPreview(t *testing.T) {
	t.Parallel()

	var _ channel.PreviewOpener = (*DiscordAdapter)(nil)
	caps := NewDiscordAdapter(nil).Descriptor().Capabilities
	if caps.Preview != channel.PreviewEditInPlace {
		t.Fatalf("preview mode = %v, want edit in place", caps.Preview)
	}
	if caps.NativeApproval {
		t.Fatal("Discord renders no approval request itself; the delivery layer sends the text prompt")
	}
}

// The preview opens as a placeholder that quotes the message it answers, and
// the final text replaces the placeholder in that same message.
func TestDiscordPreviewFinishEditsThePlaceholderInPlace(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, &channel.ReplyRef{MessageID: "src-1"})
	mustUpdate(t, p, started())
	mustUpdate(t, p, textDelta("hello "))
	mustFinish(t, p, finalOf(channel.Message{Text: "hello world"}))

	if got := api.summary(); !equalStrings(got, "POST /channels/ch-1/messages", "PATCH /channels/ch-1/messages/msg-1") {
		t.Fatalf("calls = %v", got)
	}
	calls := api.snapshot()
	if calls[0].content() != discordThinkingText {
		t.Fatalf("placeholder = %q", calls[0].content())
	}
	ref, _ := calls[0].payload(t)["message_reference"].(map[string]any)
	if ref["message_id"] != "src-1" {
		t.Fatalf("message_reference = %#v, want the source message", ref)
	}
	if calls[1].content() != "hello world" {
		t.Fatalf("final edit = %q", calls[1].content())
	}
}

// A reply that carries on after a split has no reply of its own to quote.
func TestDiscordPreviewContinuationDoesNotQuote(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil)
	mustUpdate(t, p, started())
	calls := api.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls = %v", api.summary())
	}
	if _, quoted := calls[0].payload(t)["message_reference"]; quoted {
		t.Fatalf("a continuation must not quote: %s", calls[0].Body)
	}
}

// Streamed text is shown by editing the open message, at most once per edit
// interval.
func TestDiscordPreviewEditsStreamedTextWhenDue(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil)
	mustUpdate(t, p, started())
	mustUpdate(t, p, textDelta("first "))
	if got := api.summary(); len(got) != 1 {
		t.Fatalf("an edit before the interval elapsed: %v", got)
	}
	p.mu.Lock()
	p.lastUpdate = time.Now().Add(-2 * discordEditInterval)
	p.mu.Unlock()
	mustUpdate(t, p, textDelta("second"))

	calls := api.snapshot()
	if len(calls) != 2 || calls[1].Method != http.MethodPatch || calls[1].content() != "first second" {
		t.Fatalf("calls = %v, want the placeholder edited to the streamed text", api.summary())
	}
}

func TestDiscordPreviewFinishWithoutPlaceholderSendsTheMessage(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, &channel.ReplyRef{MessageID: "src-1"})
	mustFinish(t, p, finalOf(channel.Message{Text: "answer"}))

	calls := api.snapshot()
	if len(calls) != 1 || calls[0].Method != http.MethodPost || calls[0].content() != "answer" {
		t.Fatalf("calls = %v", api.summary())
	}
}

// Text beyond the platform's size is cut to it; splitting a long reply across
// messages is the manager's work.
func TestDiscordPreviewCutsTextToThePlatformSize(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil)
	mustFinish(t, p, finalOf(channel.Message{Text: strings.Repeat("a", discordMaxLength+500)}))

	got := api.callAt(t, 0).content()
	if len([]rune(got)) != discordMaxLength || !strings.HasSuffix(got, "...") {
		t.Fatalf("sent %d runes ending %q", len([]rune(got)), got[len(got)-5:])
	}
}

func TestDiscordPreviewFinishSendsAttachments(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, &channel.ReplyRef{MessageID: "src-1"})
	mustUpdate(t, p, started())
	mustFinish(t, p, finalOf(channel.Message{Text: "see file"}, channel.PreparedAttachment{
		Logical: channel.Attachment{Type: channel.AttachmentFile, Name: "notes.txt"},
		Kind:    channel.PreparedAttachmentUpload,
		Name:    "notes.txt",
		Open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("file body")), nil
		},
	}))

	calls := api.snapshot()
	if len(calls) != 3 || calls[1].Method != http.MethodPatch || calls[2].Method != http.MethodPost {
		t.Fatalf("calls = %v, want placeholder, text edit, upload", api.summary())
	}
	if !strings.HasPrefix(calls[2].ContentType, "multipart/form-data") ||
		!strings.Contains(calls[2].Body, `filename="notes.txt"`) || !strings.Contains(calls[2].Body, "file body") {
		t.Fatalf("upload = %q %s", calls[2].ContentType, calls[2].Body)
	}
}

func TestDiscordPreviewFinishUploadsAttachmentsOfAFinalWithoutText(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil)
	mustUpdate(t, p, started())
	mustFinish(t, p, finalOf(channel.Message{}, channel.PreparedAttachment{
		Logical: channel.Attachment{Type: channel.AttachmentFile, Name: "a.txt"},
		Kind:    channel.PreparedAttachmentUpload,
		Name:    "a.txt",
		Open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("a")), nil
		},
	}))

	if got := api.summary(); !equalStrings(got, "POST /channels/ch-1/messages", "DELETE /channels/ch-1/messages/msg-1", "POST /channels/ch-1/messages") {
		t.Fatalf("calls = %v, want the placeholder removed and the file sent", got)
	}
}

// An error shown before any text is the reply's end: it takes the place of
// the placeholder, and nothing removes it afterwards.
func TestDiscordPreviewErrorReplacesThePlaceholder(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil)
	mustUpdate(t, p, started())
	mustUpdate(t, p, channel.PreparedStreamEvent{Type: channel.StreamEventError, Error: "The workspace is unreachable.", ErrorCode: "workspace.unreachable"})
	mustFinish(t, p, nil)

	calls := api.snapshot()
	if len(calls) != 2 || calls[1].Method != http.MethodPatch || calls[1].content() != "The workspace is unreachable." {
		t.Fatalf("calls = %v", api.summary())
	}
}

func TestDiscordPreviewErrorReply(t *testing.T) {
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
				sent = append(sent, c.content())
			}
			if !equalStrings(sent, tc.want...) {
				t.Fatalf("sent messages = %q, want %q", sent, tc.want)
			}
		})
	}
}

func TestDiscordPreviewFinishUsesPartsRenderer(t *testing.T) {
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

	want := "**Hello**\n\n[docs](https://example.test/a)"
	if got := api.callAt(t, 0).content(); got != want {
		t.Fatalf("Discord preview rich content mismatch\n  got:  %q\n  want: %q", got, want)
	}
}

func TestRenderDiscordStreamFinalTextUsesAuthoritativeTextBeforeBuffer(t *testing.T) {
	t.Parallel()

	got := renderDiscordStreamFinalText(channel.Message{Text: "final answer"}, "partial buffered text")
	if got != "final answer" {
		t.Fatalf("expected authoritative final text, got %q", got)
	}
}

func TestRenderDiscordStreamFinalTextFallsBackWhenEscapedRichOverflows(t *testing.T) {
	t.Parallel()

	text := strings.Repeat(`\`, discordMaxLength)
	msg := channel.Message{Parts: []channel.MessagePart{{
		Type: channel.MessagePartText,
		Text: text,
	}}}
	if got := renderDiscordStreamFinalText(msg, ""); got != text {
		t.Fatalf("expected plain fallback when escaped rich overflows, got len=%d", len([]rune(got)))
	}
}

func TestDiscordPreviewFinishPreservesURLActionsOnEdit(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil)
	mustUpdate(t, p, started())
	mustFinish(t, p, finalOf(channel.Message{
		Text:    "Read this",
		Actions: []channel.Action{{Label: "Open docs", URL: "https://example.test/docs"}},
	}))

	var edit discordCall
	for _, c := range api.snapshot() {
		if c.Method == http.MethodPatch {
			edit = c
		}
	}
	payload := edit.payload(t)
	components, ok := payload["components"].([]any)
	if !ok || len(components) != 1 {
		t.Fatalf("expected one component row, got %#v", payload["components"])
	}
	row, ok := components[0].(map[string]any)
	if !ok {
		t.Fatalf("expected component row object, got %#v", components[0])
	}
	buttons, ok := row["components"].([]any)
	if !ok || len(buttons) != 1 {
		t.Fatalf("expected one button, got %#v", row["components"])
	}
	button, ok := buttons[0].(map[string]any)
	if !ok {
		t.Fatalf("expected button object, got %#v", buttons[0])
	}
	if button["label"] != "Open docs" || button["url"] != "https://example.test/docs" || button["style"] != float64(discordgo.LinkButton) {
		t.Fatalf("unexpected button payload: %#v", button)
	}
	allowed, ok := payload["allowed_mentions"].(map[string]any)
	if !ok {
		t.Fatalf("expected explicit allowed_mentions on edit, got %#v", payload["allowed_mentions"])
	}
	if parse, ok := allowed["parse"].([]any); !ok || len(parse) != 0 {
		t.Fatalf("expected parse=[] to suppress ambient mentions, got %#v", allowed["parse"])
	}
}

// Text a tool call interrupts is kept as its own message, and the tool call
// is one message that its end edits.
func TestDiscordPreviewShowsToolCallsInOneMessage(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil)
	mustUpdate(t, p, started())
	mustUpdate(t, p, textDelta("checking"))
	call := &channel.StreamToolCall{Name: "read", CallID: "c1", Input: map[string]any{"path": "/tmp/a"}}
	mustUpdate(t, p, channel.PreparedStreamEvent{Type: channel.StreamEventToolCallStart, ToolCall: call})
	done := *call
	done.Result = map[string]any{"ok": true}
	mustUpdate(t, p, channel.PreparedStreamEvent{Type: channel.StreamEventToolCallEnd, ToolCall: &done})

	want := []string{
		"POST /channels/ch-1/messages",        // placeholder
		"PATCH /channels/ch-1/messages/msg-1", // text before the tool
		"POST /channels/ch-1/messages",        // tool call, running
		"PATCH /channels/ch-1/messages/msg-2", // tool call, finished
	}
	if got := api.summary(); !equalStrings(got, want...) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if got := api.callAt(t, 1).content(); got != "checking" {
		t.Fatalf("text before the tool = %q", got)
	}
}

// A final with no message of its own leaves the streamed text as the reply,
// including text the edit interval had held back.
func TestDiscordPreviewEmptyFinalKeepsBufferedText(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil)
	mustUpdate(t, p, started())
	mustUpdate(t, p, textDelta("held back"))
	mustFinish(t, p, finalOf(channel.Message{}))

	calls := api.snapshot()
	if len(calls) != 2 || calls[1].Method != http.MethodPatch || calls[1].content() != "held back" {
		t.Fatalf("calls = %v", api.summary())
	}
}

// A nil final means the run said nothing: the placeholder goes. Text or an
// error already shown stays.
func TestDiscordPreviewFinishWithoutFinalRemovesOnlyThePlaceholder(t *testing.T) {
	t.Parallel()

	t.Run("placeholder is removed", func(t *testing.T) {
		t.Parallel()
		p, api := newTestPreview(t, nil)
		mustUpdate(t, p, started())
		mustFinish(t, p, nil)
		if got := api.summary(); !equalStrings(got, "POST /channels/ch-1/messages", "DELETE /channels/ch-1/messages/msg-1") {
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
		p.mu.Lock()
		p.lastUpdate = time.Now().Add(-2 * discordEditInterval)
		p.mu.Unlock()
		mustUpdate(t, p, textDelta("draft"))
		mustFinish(t, p, nil)
		if got := api.summary(); !equalStrings(got, "POST /channels/ch-1/messages", "PATCH /channels/ch-1/messages/msg-1") {
			t.Fatalf("calls = %v", got)
		}
	})
}

func TestDiscordPreviewFinishTakesEffectOnce(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil)
	mustUpdate(t, p, started())
	mustFinish(t, p, finalOf(channel.Message{Text: "answer"}))
	mustFinish(t, p, finalOf(channel.Message{Text: "again"}))
	mustFinish(t, p, nil)
	mustUpdate(t, p, textDelta("late"))

	if got := api.summary(); !equalStrings(got, "POST /channels/ch-1/messages", "PATCH /channels/ch-1/messages/msg-1") {
		t.Fatalf("calls = %v, want one placeholder and one edit", got)
	}
}

// Abort adds nothing, and nothing is shown after it.
func TestDiscordPreviewAbortShowsNothingMore(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil)
	mustUpdate(t, p, started())
	if err := p.Abort(context.Background()); err != nil {
		t.Fatalf("abort: %v", err)
	}
	mustUpdate(t, p, textDelta("late"))
	mustFinish(t, p, finalOf(channel.Message{Text: "late"}))

	if got := api.summary(); !equalStrings(got, "POST /channels/ch-1/messages") {
		t.Fatalf("calls = %v, want the placeholder only", got)
	}
}
