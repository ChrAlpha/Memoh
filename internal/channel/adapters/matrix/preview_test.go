package matrix

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/felinics/memoh/internal/channel"
)

func mustPreparedMatrixEvent(t *testing.T, event channel.StreamEvent) channel.PreparedStreamEvent {
	t.Helper()
	prepared, err := channel.PrepareStreamEvent(context.Background(), nil, channel.ChannelConfig{
		BotID:       "bot-test",
		ChannelType: Type,
	}, event)
	if err != nil {
		t.Fatalf("prepare matrix stream event: %v", err)
	}
	return prepared
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// matrixHomeserver stands in for the homeserver: it records the body of every
// event sent and answers each with a new event id.
type matrixHomeserver struct {
	mu     sync.Mutex
	bodies []string
}

func (h *matrixHomeserver) RoundTrip(req *http.Request) (*http.Response, error) {
	var payload []byte
	if req.Body != nil {
		payload, _ = io.ReadAll(req.Body)
	}
	h.mu.Lock()
	h.bodies = append(h.bodies, string(payload))
	id := len(h.bodies)
	h.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"event_id":"$evt%d"}`, id))),
		Header:     make(http.Header),
	}, nil
}

func (h *matrixHomeserver) sent() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.bodies...)
}

// content decodes the i-th event sent.
func (h *matrixHomeserver) content(t *testing.T, i int) map[string]any {
	t.Helper()
	sent := h.sent()
	if i >= len(sent) {
		t.Fatalf("event %d missing, %d sent", i, len(sent))
	}
	var content map[string]any
	if err := json.Unmarshal([]byte(sent[i]), &content); err != nil {
		t.Fatalf("decode event %d: %v (body=%q)", i, err, sent[i])
	}
	return content
}

func newTestPreview(t *testing.T, reply *channel.ReplyRef) (*matrixPreview, *matrixHomeserver) {
	t.Helper()
	server := &matrixHomeserver{}
	adapter := NewMatrixAdapter(nil)
	adapter.httpClient = &http.Client{Transport: server}
	return &matrixPreview{
		adapter: adapter,
		cfg: Config{
			HomeserverURL: "https://matrix.example.com",
			AccessToken:   "tok",
		},
		target: "!room:example.com",
		reply:  reply,
	}, server
}

func mustUpdate(t *testing.T, p *matrixPreview, event channel.StreamEvent) {
	t.Helper()
	if err := p.Update(context.Background(), mustPreparedMatrixEvent(t, event)); err != nil {
		t.Fatalf("update %s: %v", event.Type, err)
	}
}

func textDelta(text string) channel.StreamEvent {
	return channel.StreamEvent{Type: channel.StreamEventDelta, Delta: text, Phase: channel.StreamPhaseText}
}

func textPhaseEnd() channel.StreamEvent {
	return channel.StreamEvent{Type: channel.StreamEventPhaseEnd, Phase: channel.StreamPhaseText}
}

func mustFinish(t *testing.T, p *matrixPreview, msg channel.Message) {
	t.Helper()
	final := mustPreparedMatrixEvent(t, channel.StreamEvent{Type: channel.StreamEventFinal, Final: &channel.StreamFinalizePayload{Message: msg}})
	if err := p.Finish(context.Background(), &final); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func TestMatrixPreviewCapabilities(t *testing.T) {
	t.Parallel()

	var _ channel.PreviewOpener = (*MatrixAdapter)(nil)
	caps := NewMatrixAdapter(nil).Descriptor().Capabilities
	if caps.Preview != channel.PreviewEditInPlace {
		t.Fatalf("preview mode = %v, want edit in place", caps.Preview)
	}
	if caps.NativeApproval {
		t.Fatal("Matrix renders no approval request itself; the delivery layer sends the text prompt")
	}
}

func TestMatrixPreviewDoesNotSendDeltaBeforeTextPhaseEnds(t *testing.T) {
	t.Parallel()

	p, server := newTestPreview(t, nil)
	mustUpdate(t, p, textDelta("draft"))
	if got := len(server.sent()); got != 0 {
		t.Fatalf("expected no request before text phase ends, got %d", got)
	}
	mustUpdate(t, p, textPhaseEnd())
	if got := len(server.sent()); got != 1 {
		t.Fatalf("expected one request after text phase end, got %d", got)
	}
}

// The text of a phase is sent as a message that quotes the one it answers; the
// final then edits that message, and the edit quotes nothing.
func TestMatrixPreviewFinishEditsTheMessageTheTextPhaseSent(t *testing.T) {
	t.Parallel()

	p, server := newTestPreview(t, &channel.ReplyRef{MessageID: "$src"})
	mustUpdate(t, p, textDelta("**bold**"))
	mustUpdate(t, p, textPhaseEnd())
	mustFinish(t, p, channel.Message{Text: "**bold**", Format: channel.MessageFormatMarkdown})

	if got := len(server.sent()); got != 2 {
		t.Fatalf("expected a send and an edit, got %d events", got)
	}
	first, edit := server.content(t, 0), server.content(t, 1)
	if _, formatted := first["formatted_body"]; formatted {
		t.Fatalf("expected plain interim send, got %v", first)
	}
	relates, _ := first["m.relates_to"].(map[string]any)
	if reply, _ := relates["m.in_reply_to"].(map[string]any); reply["event_id"] != "$src" {
		t.Fatalf("first message relates_to = %#v, want a reply to the source message", relates)
	}
	if _, formatted := edit["formatted_body"]; !formatted || edit["format"] != matrixHTMLFormat {
		t.Fatalf("expected markdown final edit, got %v", edit)
	}
	relates, _ = edit["m.relates_to"].(map[string]any)
	if relates["rel_type"] != "m.replace" || relates["event_id"] != "$evt1" {
		t.Fatalf("edit relates_to = %#v, want a replacement of the first event", relates)
	}
}

func TestMatrixPreviewFlushesBufferedTextWhenToolStarts(t *testing.T) {
	t.Parallel()

	p, server := newTestPreview(t, nil)
	mustUpdate(t, p, textDelta("I will inspect first"))
	tcStart := &channel.StreamToolCall{Name: "read", CallID: "c1", Input: map[string]any{"path": "/tmp/a"}}
	tcEnd := &channel.StreamToolCall{Name: "read", CallID: "c1", Input: map[string]any{"path": "/tmp/a"}, Result: map[string]any{"ok": true}}
	mustUpdate(t, p, channel.StreamEvent{Type: channel.StreamEventToolCallStart, ToolCall: tcStart})
	if got := len(server.sent()); got != 2 {
		t.Fatalf("expected flush + start messages, got %d", got)
	}
	mustUpdate(t, p, channel.StreamEvent{Type: channel.StreamEventToolCallEnd, ToolCall: tcEnd})
	if got := len(server.sent()); got != 3 {
		t.Fatalf("expected start + end tool messages plus flush, got %d", got)
	}
	relates, _ := server.content(t, 2)["m.relates_to"].(map[string]any)
	if relates["rel_type"] != "m.replace" || relates["event_id"] != "$evt2" {
		t.Fatalf("tool end relates_to = %#v, want an edit of the tool call message", relates)
	}
	mustUpdate(t, p, textDelta("Final answer"))
	mustFinish(t, p, channel.Message{Text: "Final answer"})
	if got := len(server.sent()); got != 4 {
		t.Fatalf("expected final visible message after tool call, got %d", got)
	}
}

func TestMatrixPreviewFinishPreservesRichPartsAsFormattedHTML(t *testing.T) {
	t.Parallel()

	p, server := newTestPreview(t, nil)
	mustFinish(t, p, channel.Message{
		Format: channel.MessageFormatRich,
		Parts: []channel.MessagePart{
			{Type: channel.MessagePartText, Text: "done", Styles: []channel.MessageTextStyle{channel.MessageStyleBold}},
			{Type: channel.MessagePartMention, Text: "Alice", ChannelIdentityID: "@alice:example.com"},
		},
	})

	if got := len(server.sent()); got != 1 {
		t.Fatalf("expected one final send, got %d", got)
	}
	content := server.content(t, 0)
	if got := content["format"]; got != matrixHTMLFormat {
		t.Fatalf("unexpected final format: %#v", got)
	}
	html, ok := content["formatted_body"].(string)
	if !ok {
		t.Fatalf("unexpected final formatted body: %#v", content["formatted_body"])
	}
	for _, want := range []string{
		`<strong>done</strong>`,
		`https://matrix.to/#/@alice:example.com`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("expected final formatted body to contain %q, got %s", want, html)
		}
	}
}

func TestMatrixPreviewFinishSendsAttachments(t *testing.T) {
	t.Parallel()

	p, server := newTestPreview(t, nil)
	mustFinish(t, p, channel.Message{
		Text: "done",
		Attachments: []channel.Attachment{{
			Type:           channel.AttachmentImage,
			PlatformKey:    "mxc://matrix.example.com/media123",
			Name:           "image.png",
			SourcePlatform: Type.String(),
		}},
	})

	sent := server.sent()
	if len(sent) != 2 {
		t.Fatalf("expected text and attachment sends, got %d", len(sent))
	}
	if !strings.Contains(sent[0], `"msgtype":"m.notice"`) {
		t.Fatalf("expected first payload to be text, got %s", sent[0])
	}
	if !strings.Contains(sent[1], `"msgtype":"m.image"`) || !strings.Contains(sent[1], `mxc://matrix.example.com/media123`) {
		t.Fatalf("expected second payload to be attachment, got %s", sent[1])
	}
}

// An error is sent as the text of a message, labelled unless it carries an
// error code; a blank error sends nothing.
func TestMatrixPreviewErrorReply(t *testing.T) {
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
			p, server := newTestPreview(t, nil)
			ctx := context.Background()
			if err := p.Update(ctx, channel.PreparedStreamEvent{Type: channel.StreamEventDelta, Delta: "draft", Phase: channel.StreamPhaseText}); err != nil {
				t.Fatalf("update delta: %v", err)
			}
			if err := p.Update(ctx, tc.event); err != nil {
				t.Fatalf("update error: %v", err)
			}
			var sent []string
			for i := range server.sent() {
				sent = append(sent, fmt.Sprint(server.content(t, i)["body"]))
			}
			if strings.Join(sent, "|") != strings.Join(tc.want, "|") || len(sent) != len(tc.want) {
				t.Fatalf("sent messages = %q, want %q", sent, tc.want)
			}
		})
	}
}

// A final with no message of its own sends the text streamed so far.
func TestMatrixPreviewEmptyFinalSendsBufferedText(t *testing.T) {
	t.Parallel()

	p, server := newTestPreview(t, nil)
	mustUpdate(t, p, textDelta("held back"))
	mustFinish(t, p, channel.Message{})

	if got := len(server.sent()); got != 1 || server.content(t, 0)["body"] != "held back" {
		t.Fatalf("sent = %q", server.sent())
	}
}

// The room shows no placeholder, so a nil final means the run said nothing and
// sends nothing, whatever text was streamed.
func TestMatrixPreviewFinishWithoutFinalSendsNothing(t *testing.T) {
	t.Parallel()

	p, server := newTestPreview(t, nil)
	mustUpdate(t, p, textDelta("NO_REPLY"))
	if err := p.Finish(context.Background(), nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if got := server.sent(); len(got) != 0 {
		t.Fatalf("sent = %q", got)
	}
}

func TestMatrixPreviewFinishTakesEffectOnce(t *testing.T) {
	t.Parallel()

	p, server := newTestPreview(t, nil)
	mustFinish(t, p, channel.Message{Text: "answer"})
	mustFinish(t, p, channel.Message{Text: "again"})
	if err := p.Finish(context.Background(), nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	mustUpdate(t, p, textPhaseEnd())

	if got := len(server.sent()); got != 1 {
		t.Fatalf("sent %d events, want the first final only", got)
	}
}

// Abort adds nothing, and nothing is shown after it.
func TestMatrixPreviewAbortShowsNothingMore(t *testing.T) {
	t.Parallel()

	p, server := newTestPreview(t, nil)
	mustUpdate(t, p, textDelta("partial"))
	if err := p.Abort(context.Background()); err != nil {
		t.Fatalf("abort: %v", err)
	}
	mustUpdate(t, p, textPhaseEnd())
	mustFinish(t, p, channel.Message{Text: "late"})

	if got := server.sent(); len(got) != 0 {
		t.Fatalf("sent = %q", got)
	}
}
