package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"

	"github.com/felinics/memoh/internal/channel"
)

func TestExtractReadableFromJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text", "hello world", "hello world"},
		{"json with text", `{"text":"extracted"}`, "extracted"},
		{"json with message", `{"message":"ok"}`, "ok"},
		{"json with content", `{"content":"result"}`, "result"},
		{"invalid json", `{invalid`, `{invalid`},
		{"empty object", `{}`, `{}`},
		{"array of strings", `["first"]`, "first"},
		{"array empty", `[]`, `[]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractReadableFromJSON(tc.in)
			if got != tc.want {
				t.Errorf("extractReadableFromJSON(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRenderFeishuStreamFinalTextUsesParts(t *testing.T) {
	t.Parallel()

	msg := channel.Message{
		Text: "plain fallback",
		Parts: []channel.MessagePart{
			{Type: channel.MessagePartText, Text: "Hello", Styles: []channel.MessageTextStyle{channel.MessageStyleBold}},
			{Type: channel.MessagePartLink, Text: "docs", URL: "https://example.test"},
		},
	}
	got := renderFeishuStreamFinalText(msg, "buffered plain text")
	want := "**Hello**\n\n[docs](https://example.test)"
	if got != want {
		t.Fatalf("expected rich parts to drive Feishu stream final\n  got:  %q\n  want: %q", got, want)
	}
}

func TestRenderFeishuStreamFinalTextLongRichPartsFallsBackToPlain(t *testing.T) {
	t.Parallel()

	got := renderFeishuStreamFinalText(channel.Message{
		Parts: []channel.MessagePart{
			{Type: channel.MessagePartText, Text: strings.Repeat("你", feishuStreamMaxRunes+100), Styles: []channel.MessageTextStyle{channel.MessageStyleBold}},
		},
	}, "buffered plain text")
	if strings.Contains(got, "**") {
		t.Fatalf("long rich stream final should fall back to plain text, got prefix %q", got[:20])
	}
	if len([]rune(got)) <= feishuStreamMaxRunes {
		t.Fatalf("render helper should return full plain fallback before patch truncation, got len=%d", len([]rune(got)))
	}
}

func TestRenderFeishuStreamFinalTextUsesAuthoritativeTextBeforeBuffer(t *testing.T) {
	t.Parallel()

	got := renderFeishuStreamFinalText(channel.Message{Text: "plain fallback"}, "buffered plain text")
	if got != "plain fallback" {
		t.Fatalf("expected authoritative final text, got %q", got)
	}
}

type feishuCall struct {
	Method string
	Path   string
	Body   string
}

// cardText is the text of the card a create, reply or patch call carries.
func (c feishuCall) cardText(t *testing.T) string {
	t.Helper()
	var body struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(c.Body), &body); err != nil {
		t.Fatalf("decode %s %s body: %v (body=%q)", c.Method, c.Path, err, c.Body)
	}
	var card struct {
		Body struct {
			Elements []struct {
				Content string `json:"content"`
			} `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(body.Content), &card); err != nil {
		t.Fatalf("decode card %q: %v", body.Content, err)
	}
	if len(card.Body.Elements) != 1 {
		t.Fatalf("unexpected card %q", body.Content)
	}
	return card.Body.Elements[0].Content
}

// feishuAPI stands in for the Feishu open API. It records the message calls
// and answers a send with a new message id.
type feishuAPI struct {
	mu    sync.Mutex
	calls []feishuCall
	sent  int
}

func (a *feishuAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if strings.Contains(r.URL.Path, "tenant_access_token") {
		_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"tenant-token","expire":7200}`)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, feishuCall{Method: r.Method, Path: r.URL.Path, Body: string(raw)})
	if r.Method == http.MethodPost {
		a.sent++
		_, _ = fmt.Fprintf(w, `{"code":0,"data":{"message_id":"om_%d"}}`, a.sent)
		return
	}
	_, _ = io.WriteString(w, `{"code":0}`)
}

func (a *feishuAPI) snapshot() []feishuCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]feishuCall{}, a.calls...)
}

// callAt is the i-th recorded call; the test stops when there is none.
func (a *feishuAPI) callAt(t *testing.T, i int) feishuCall {
	t.Helper()
	calls := a.snapshot()
	if i >= len(calls) {
		t.Fatalf("call %d missing, calls = %v", i, a.summary())
	}
	return calls[i]
}

// summary lists the calls as "METHOD path".
func (a *feishuAPI) summary() []string {
	calls := a.snapshot()
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Method+" "+strings.TrimPrefix(c.Path, "/open-apis/im/v1"))
	}
	return out
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

// newTestPreview opens a preview against a fake open API. patchInterval is
// how long a streamed delta waits before it is patched into the card.
func newTestPreview(t *testing.T, reply *channel.ReplyRef, patchInterval time.Duration) (*feishuPreview, *feishuAPI) {
	t.Helper()
	api := &feishuAPI{}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	return &feishuPreview{
		adapter:       &FeishuAdapter{},
		cfg:           channel.ChannelConfig{ID: "cfg-1"},
		target:        "chat_id:oc_1",
		reply:         reply,
		client:        lark.NewClient("app-"+t.Name(), "app-secret", lark.WithOpenBaseUrl(server.URL)),
		receiveID:     "oc_1",
		receiveType:   "chat_id",
		patchInterval: patchInterval,
		sendMessage: func(context.Context, channel.ChannelConfig, channel.PreparedOutboundMessage) error {
			return nil
		},
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

func mustUpdate(t *testing.T, p *feishuPreview, event channel.PreparedStreamEvent) {
	t.Helper()
	if err := p.Update(context.Background(), event); err != nil {
		t.Fatalf("update %s: %v", event.Type, err)
	}
}

func mustFinish(t *testing.T, p *feishuPreview, final *channel.PreparedStreamEvent) {
	t.Helper()
	if err := p.Finish(context.Background(), final); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func TestFeishuPreviewCapabilities(t *testing.T) {
	t.Parallel()

	var _ channel.PreviewOpener = (*FeishuAdapter)(nil)
	caps := NewFeishuAdapter(nil).Descriptor().Capabilities
	if caps.Preview != channel.PreviewEditInPlace {
		t.Fatalf("preview mode = %v, want edit in place", caps.Preview)
	}
	if caps.NativeApproval {
		t.Fatal("Feishu renders no approval request itself; the delivery layer sends the text prompt")
	}
	if caps.ReactionRemoveKey != channel.ReactionRemoveByReactionID {
		t.Fatalf("reaction remove key = %v, want the reaction id", caps.ReactionRemoveKey)
	}
}

// The card opens as a placeholder that replies to the message it answers, and
// the final text patches that card.
func TestFeishuPreviewFinishPatchesThePlaceholderCard(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, &channel.ReplyRef{MessageID: "om_src"}, time.Hour)
	mustUpdate(t, p, started())
	mustUpdate(t, p, textDelta("hello "))
	mustFinish(t, p, finalOf(channel.Message{Text: "hello world"}))

	if got := api.summary(); !equalStrings(got, "POST /messages/om_src/reply", "PATCH /messages/om_1") {
		t.Fatalf("calls = %v", got)
	}
	calls := api.snapshot()
	if calls[0].cardText(t) != feishuStreamThinkingText || calls[1].cardText(t) != "hello world" {
		t.Fatalf("placeholder %q, final %q", calls[0].cardText(t), calls[1].cardText(t))
	}
}

func TestFeishuPreviewWithoutReplyCreatesTheCardInTheChat(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil, time.Hour)
	mustUpdate(t, p, started())

	if got := api.summary(); !equalStrings(got, "POST /messages") {
		t.Fatalf("calls = %v", got)
	}
}

// Streamed text is patched into the card once the patch interval has passed.
func TestFeishuPreviewPatchesStreamedText(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil, 0)
	mustUpdate(t, p, started())
	mustUpdate(t, p, textDelta("first "))
	mustUpdate(t, p, textDelta("second"))

	calls := api.snapshot()
	if len(calls) != 3 || calls[2].Method != http.MethodPatch || calls[2].cardText(t) != "first second" {
		t.Fatalf("calls = %v", api.summary())
	}
}

// A final with no message of its own leaves the streamed text as the reply,
// including text the patch interval held back.
func TestFeishuPreviewEmptyFinalKeepsBufferedText(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil, time.Hour)
	mustUpdate(t, p, started())
	mustUpdate(t, p, textDelta("held back"))
	mustFinish(t, p, finalOf(channel.Message{}))

	calls := api.snapshot()
	if len(calls) != 2 || calls[1].Method != http.MethodPatch || calls[1].cardText(t) != "held back" {
		t.Fatalf("calls = %v", api.summary())
	}
}

// Text beyond what a card shows keeps its tail; splitting a long reply across
// messages is the manager's work.
func TestFeishuPreviewKeepsTheTailOfLongText(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil, time.Hour)
	long := strings.Repeat("a", feishuStreamMaxRunes) + "tail"
	mustFinish(t, p, finalOf(channel.Message{Text: long}))

	got := api.callAt(t, len(api.snapshot())-1).cardText(t)
	if !strings.HasPrefix(got, "...") || !strings.HasSuffix(got, "tail") || len([]rune(got)) > feishuStreamMaxRunes+4 {
		t.Fatalf("card text has %d runes, prefix %q", len([]rune(got)), got[:8])
	}
}

func TestFeishuPreviewFinishSendsAttachments(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil, time.Hour)
	var sent []channel.PreparedOutboundMessage
	p.sendMessage = func(_ context.Context, _ channel.ChannelConfig, msg channel.PreparedOutboundMessage) error {
		sent = append(sent, msg)
		return nil
	}
	att := channel.PreparedAttachment{
		Logical:   channel.Attachment{Type: channel.AttachmentImage, Name: "a.png"},
		Kind:      channel.PreparedAttachmentNativeRef,
		NativeRef: "img_key",
	}
	mustUpdate(t, p, started())
	mustFinish(t, p, finalOf(channel.Message{Text: "see image", Format: channel.MessageFormatMarkdown}, att))

	if got := api.summary(); !equalStrings(got, "POST /messages", "PATCH /messages/om_1") {
		t.Fatalf("calls = %v", got)
	}
	if len(sent) != 1 || sent[0].Target != "chat_id:oc_1" || len(sent[0].Message.Attachments) != 1 ||
		sent[0].Message.Message.Text != "" || len(sent[0].Message.Message.Attachments) != 1 {
		t.Fatalf("attachment messages = %+v, want one media message without the text", sent)
	}
}

func TestFeishuPreviewFinishRecallsThePlaceholderOfAnAttachmentOnlyFinal(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil, time.Hour)
	var sent int
	p.sendMessage = func(context.Context, channel.ChannelConfig, channel.PreparedOutboundMessage) error {
		sent++
		return nil
	}
	mustUpdate(t, p, started())
	mustFinish(t, p, finalOf(channel.Message{}, channel.PreparedAttachment{
		Logical:   channel.Attachment{Type: channel.AttachmentImage},
		Kind:      channel.PreparedAttachmentNativeRef,
		NativeRef: "img_key",
	}))

	if got := api.summary(); !equalStrings(got, "POST /messages", "DELETE /messages/om_1") || sent != 1 {
		t.Fatalf("calls = %v, attachment messages = %d", got, sent)
	}
}

// An error shown before any text is the reply's end: it takes the place of
// the placeholder, and nothing removes it afterwards.
func TestFeishuPreviewErrorReplacesThePlaceholder(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil, time.Hour)
	mustUpdate(t, p, started())
	mustUpdate(t, p, channel.PreparedStreamEvent{Type: channel.StreamEventError, Error: "The workspace is unreachable.", ErrorCode: "workspace.unreachable"})
	mustFinish(t, p, nil)

	calls := api.snapshot()
	if len(calls) != 2 || calls[1].Method != http.MethodPatch || calls[1].cardText(t) != "The workspace is unreachable." {
		t.Fatalf("calls = %v", api.summary())
	}
}

func TestFeishuPreviewErrorReply(t *testing.T) {
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
			p, api := newTestPreview(t, nil, time.Hour)
			p.cardMessageID = "om_card"
			p.lastPatched = "draft"
			mustUpdate(t, p, tc.event)

			var patched []string
			for _, c := range api.snapshot() {
				if c.Method != http.MethodPatch || c.Path != "/open-apis/im/v1/messages/om_card" {
					t.Fatalf("unexpected request %s %s", c.Method, c.Path)
				}
				patched = append(patched, c.cardText(t))
			}
			if !equalStrings(patched, tc.want...) {
				t.Fatalf("patched cards = %q, want %q", patched, tc.want)
			}
		})
	}
}

func TestFeishuPreviewFinishUsesPartsRenderer(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil, time.Hour)
	mustFinish(t, p, finalOf(channel.Message{
		Text:   "plain fallback",
		Format: channel.MessageFormatRich,
		Parts: []channel.MessagePart{
			{Type: channel.MessagePartText, Text: "Hello", Styles: []channel.MessageTextStyle{channel.MessageStyleBold}},
			{Type: channel.MessagePartLink, Text: "docs", URL: "https://example.test"},
		},
	}))

	calls := api.snapshot()
	if got, want := calls[len(calls)-1].cardText(t), "**Hello**\n\n[docs](https://example.test)"; got != want {
		t.Fatalf("card text = %q, want %q", got, want)
	}
}

// A tool call is one card that its end patches; text before it is kept as its
// own card.
func TestFeishuPreviewShowsToolCallsInOneCard(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil, time.Hour)
	mustUpdate(t, p, started())
	mustUpdate(t, p, textDelta("checking"))
	call := &channel.StreamToolCall{Name: "read", CallID: "c1", Input: map[string]any{"path": "/tmp/a"}}
	mustUpdate(t, p, channel.PreparedStreamEvent{Type: channel.StreamEventToolCallStart, ToolCall: call})
	done := *call
	done.Result = map[string]any{"ok": true}
	mustUpdate(t, p, channel.PreparedStreamEvent{Type: channel.StreamEventToolCallEnd, ToolCall: &done})

	want := []string{
		"POST /messages",       // placeholder
		"PATCH /messages/om_1", // text before the tool
		"POST /messages",       // tool call, running
		"PATCH /messages/om_2", // tool call, finished
	}
	if got := api.summary(); !equalStrings(got, want...) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if got := api.callAt(t, 1).cardText(t); got != "checking" {
		t.Fatalf("text before the tool = %q", got)
	}
}

// A nil final means the run said nothing: the placeholder card is recalled.
// Text or an error already shown stays.
func TestFeishuPreviewFinishWithoutFinalRecallsOnlyThePlaceholder(t *testing.T) {
	t.Parallel()

	t.Run("placeholder is recalled", func(t *testing.T) {
		t.Parallel()
		p, api := newTestPreview(t, nil, time.Hour)
		mustUpdate(t, p, started())
		mustFinish(t, p, nil)
		if got := api.summary(); !equalStrings(got, "POST /messages", "DELETE /messages/om_1") {
			t.Fatalf("calls = %v", got)
		}
	})
	t.Run("nothing open sends nothing", func(t *testing.T) {
		t.Parallel()
		p, api := newTestPreview(t, nil, time.Hour)
		mustFinish(t, p, nil)
		if got := api.summary(); len(got) != 0 {
			t.Fatalf("calls = %v", got)
		}
	})
	t.Run("streamed text stays", func(t *testing.T) {
		t.Parallel()
		p, api := newTestPreview(t, nil, 0)
		mustUpdate(t, p, started())
		mustUpdate(t, p, textDelta("draft"))
		mustFinish(t, p, nil)
		if got := api.summary(); !equalStrings(got, "POST /messages", "PATCH /messages/om_1") {
			t.Fatalf("calls = %v", got)
		}
	})
}

func TestFeishuPreviewFinishTakesEffectOnce(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil, time.Hour)
	mustUpdate(t, p, started())
	mustFinish(t, p, finalOf(channel.Message{Text: "answer"}))
	mustFinish(t, p, finalOf(channel.Message{Text: "again"}))
	mustFinish(t, p, nil)
	mustUpdate(t, p, textDelta("late"))

	if got := api.summary(); !equalStrings(got, "POST /messages", "PATCH /messages/om_1") {
		t.Fatalf("calls = %v, want one placeholder and one patch", got)
	}
}

// Abort adds nothing, and nothing is shown after it.
func TestFeishuPreviewAbortShowsNothingMore(t *testing.T) {
	t.Parallel()

	p, api := newTestPreview(t, nil, time.Hour)
	mustUpdate(t, p, started())
	if err := p.Abort(context.Background()); err != nil {
		t.Fatalf("abort: %v", err)
	}
	mustUpdate(t, p, textDelta("late"))
	mustFinish(t, p, finalOf(channel.Message{Text: "late"}))

	if got := api.summary(); !equalStrings(got, "POST /messages") {
		t.Fatalf("calls = %v, want the placeholder only", got)
	}
}
