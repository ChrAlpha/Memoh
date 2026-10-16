package qq

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/channel"
)

func preparedQQEvent(event channel.StreamEvent) channel.PreparedStreamEvent {
	prepared := channel.PreparedStreamEvent{
		Type:      event.Type,
		Delta:     event.Delta,
		Error:     event.Error,
		ErrorCode: event.ErrorCode,
		Status:    event.Status,
		Phase:     event.Phase,
	}
	if len(event.Attachments) > 0 {
		prepared.Attachments = make([]channel.PreparedAttachment, 0, len(event.Attachments))
		for _, att := range event.Attachments {
			prepared.Attachments = append(prepared.Attachments, channel.PreparedAttachment{
				Logical:   att,
				Kind:      channel.PreparedAttachmentUpload,
				Name:      att.Name,
				Mime:      att.Mime,
				PublicURL: att.URL,
				Open: func(context.Context) (io.ReadCloser, error) {
					return io.NopCloser(strings.NewReader("test")), nil
				},
			})
		}
	}
	if event.Final != nil {
		final := channel.PreparedMessage{Message: event.Final.Message}
		for _, att := range event.Final.Message.Attachments {
			final.Attachments = append(final.Attachments, channel.PreparedAttachment{
				Logical: att,
				Kind:    channel.PreparedAttachmentUpload,
				Name:    att.Name,
				Mime:    att.Mime,
				Open: func(context.Context) (io.ReadCloser, error) {
					return io.NopCloser(strings.NewReader("test")), nil
				},
			})
		}
		prepared.Final = &channel.PreparedStreamFinalizePayload{Message: final}
	}
	return prepared
}

// drive delivers an event the way the manager does: a final ends the preview
// with Finish, a completed status ends it with no final, and anything else
// updates it.
func (s *qqPreview) drive(ctx context.Context, event channel.PreparedStreamEvent) error {
	switch {
	case event.Type == channel.StreamEventFinal && event.Final != nil:
		return s.Finish(ctx, &event)
	case event.Type == channel.StreamEventStatus && event.Status == channel.StreamStatusCompleted:
		return s.Finish(ctx, nil)
	default:
		return s.Update(ctx, event)
	}
}

func TestQQPreviewFlushesBufferedTextOnFinal(t *testing.T) {
	t.Parallel()

	var sent []channel.OutboundMessage
	stream := &qqPreview{
		target: "c2c:user-openid",
		send: func(_ context.Context, msg channel.PreparedOutboundMessage) error {
			sent = append(sent, msg.LogicalMessage())
			return nil
		},
	}

	ctx := context.Background()
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventStatus, Status: channel.StreamStatusStarted})); err != nil {
		t.Fatalf("push status: %v", err)
	}
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "Hi "})); err != nil {
		t.Fatalf("push delta1: %v", err)
	}
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "there"})); err != nil {
		t.Fatalf("push delta2: %v", err)
	}
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventFinal, Final: &channel.StreamFinalizePayload{}})); err != nil {
		t.Fatalf("push final: %v", err)
	}

	if len(sent) != 1 {
		t.Fatalf("expected one send, got %d", len(sent))
	}
	if sent[0].Target != "c2c:user-openid" {
		t.Fatalf("unexpected target: %s", sent[0].Target)
	}
	if sent[0].Message.PlainText() != "Hi there" {
		t.Fatalf("unexpected text: %q", sent[0].Message.PlainText())
	}
}

func TestQQPreviewFinalSendsTextAndAttachments(t *testing.T) {
	t.Parallel()

	var sent []channel.OutboundMessage
	stream := &qqPreview{
		target: "group:group-openid",
		send: func(_ context.Context, msg channel.PreparedOutboundMessage) error {
			sent = append(sent, msg.LogicalMessage())
			return nil
		},
	}

	if err := stream.drive(context.Background(), preparedQQEvent(channel.StreamEvent{
		Type: channel.StreamEventFinal,
		Final: &channel.StreamFinalizePayload{Message: channel.Message{
			Text:        "done",
			Attachments: []channel.Attachment{{Type: channel.AttachmentImage, URL: "https://example.com/a.png"}},
		}},
	})); err != nil {
		t.Fatalf("push final: %v", err)
	}

	if len(sent) != 1 {
		t.Fatalf("expected one send, got %d", len(sent))
	}
	if sent[0].Message.PlainText() != "done" {
		t.Fatalf("unexpected text: %q", sent[0].Message.PlainText())
	}
	if len(sent[0].Message.Attachments) != 1 {
		t.Fatalf("unexpected attachments: %d", len(sent[0].Message.Attachments))
	}
}

func TestQQPreviewFinalPrefersBufferedVisibleText(t *testing.T) {
	t.Parallel()

	var sent []channel.OutboundMessage
	stream := &qqPreview{
		target: "c2c:user-openid",
		send: func(_ context.Context, msg channel.PreparedOutboundMessage) error {
			sent = append(sent, msg.LogicalMessage())
			return nil
		},
	}

	ctx := context.Background()
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "visible "})); err != nil {
		t.Fatalf("push delta1: %v", err)
	}
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "answer"})); err != nil {
		t.Fatalf("push delta2: %v", err)
	}
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{
		Type: channel.StreamEventFinal,
		Final: &channel.StreamFinalizePayload{Message: channel.Message{
			Text: "internal trace\nvisible answer",
		}},
	})); err != nil {
		t.Fatalf("push final: %v", err)
	}

	if len(sent) != 1 {
		t.Fatalf("expected one send, got %d", len(sent))
	}
	if got := sent[0].Message.PlainText(); got != "visible answer" {
		t.Fatalf("unexpected text: %q", got)
	}
}

// A second Finish, and an Abort after the reply has ended, send nothing more.
func TestQQPreviewFinishEndsTheReplyOnce(t *testing.T) {
	t.Parallel()

	var sent []channel.OutboundMessage
	stream := &qqPreview{
		target: "c2c:user-openid",
		send: func(_ context.Context, msg channel.PreparedOutboundMessage) error {
			sent = append(sent, msg.LogicalMessage())
			return nil
		},
	}

	ctx := context.Background()
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "visible answer"})); err != nil {
		t.Fatalf("push delta: %v", err)
	}
	for _, text := range []string{"", "a later final"} {
		if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{
			Type:  channel.StreamEventFinal,
			Final: &channel.StreamFinalizePayload{Message: channel.Message{Text: text}},
		})); err != nil {
			t.Fatalf("push final: %v", err)
		}
	}
	if err := stream.Abort(ctx); err != nil {
		t.Fatalf("abort: %v", err)
	}

	if len(sent) != 1 {
		t.Fatalf("expected 1 outbound message, got %d", len(sent))
	}
	if got := sent[0].Message.PlainText(); got != "visible answer" {
		t.Fatalf("unexpected text: %q", got)
	}
}

func TestQQPreviewRejectsUpdateAfterAbort(t *testing.T) {
	t.Parallel()

	stream := &qqPreview{send: func(context.Context, channel.PreparedOutboundMessage) error { return nil }}
	if err := stream.Abort(context.Background()); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if err := stream.drive(context.Background(), preparedQQEvent(channel.StreamEvent{
		Type:  channel.StreamEventDelta,
		Delta: "x",
	})); err == nil {
		t.Fatal("expected closed error")
	}
}

func TestQQPreviewErrorReply(t *testing.T) {
	t.Parallel()

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
			var sent []string
			stream := &qqPreview{
				target: "c2c:user-openid",
				send: func(_ context.Context, msg channel.PreparedOutboundMessage) error {
					sent = append(sent, msg.LogicalMessage().Message.PlainText())
					return nil
				},
			}
			if err := stream.drive(context.Background(), preparedQQEvent(tc.event)); err != nil {
				t.Fatalf("push error: %v", err)
			}
			if len(sent) != len(tc.want) || strings.Join(sent, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("sent messages = %q, want %q", sent, tc.want)
			}
		})
	}
}

func TestQQPreviewC2CStreamsCumulativeShards(t *testing.T) {
	t.Parallel()

	var shards []qqStreamShardRequest
	var sent []channel.OutboundMessage
	stream := &qqPreview{
		target:         "c2c:user-openid",
		streamInterval: 0,
		now:            time.Now,
		send: func(_ context.Context, msg channel.PreparedOutboundMessage) error {
			sent = append(sent, msg.LogicalMessage())
			return nil
		},
		streamSend: func(_ context.Context, req qqStreamShardRequest) (qqStreamShardResponse, error) {
			shards = append(shards, req)
			return qqStreamShardResponse{ID: "sm-1"}, nil
		},
	}

	ctx := context.Background()
	for _, delta := range []string{"你好", "，世界"} {
		if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: delta})); err != nil {
			t.Fatalf("push delta: %v", err)
		}
	}
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventFinal, Final: &channel.StreamFinalizePayload{}})); err != nil {
		t.Fatalf("push final: %v", err)
	}

	if len(sent) != 0 {
		t.Fatalf("expected no regular sends, got %d", len(sent))
	}
	if len(shards) != 3 {
		t.Fatalf("expected 3 shards, got %d", len(shards))
	}
	if shards[0].StreamMsgID != "" || shards[0].Index != 0 || shards[0].InputState != qqStreamInputGenerating {
		t.Fatalf("unexpected first shard: %+v", shards[0])
	}
	if shards[0].ContentRaw != "你好" || shards[1].ContentRaw != "你好，世界" {
		t.Fatalf("shards must carry cumulative text: %+v", shards)
	}
	for i, shard := range shards {
		if shard.Index != i {
			t.Fatalf("shard %d index = %d", i, shard.Index)
		}
		if i > 0 && shard.StreamMsgID != "sm-1" {
			t.Fatalf("shard %d must reuse server stream id: %+v", i, shard)
		}
	}
	if shards[2].InputState != qqStreamInputDone {
		t.Fatalf("final shard state = %d", shards[2].InputState)
	}
}

func TestQQPreviewC2CShardFailureFallsBackToRegularSend(t *testing.T) {
	t.Parallel()

	var sent []channel.OutboundMessage
	stream := &qqPreview{
		target:         "c2c:user-openid",
		streamInterval: 0,
		now:            time.Now,
		send: func(_ context.Context, msg channel.PreparedOutboundMessage) error {
			sent = append(sent, msg.LogicalMessage())
			return nil
		},
		streamSend: func(context.Context, qqStreamShardRequest) (qqStreamShardResponse, error) {
			return qqStreamShardResponse{}, errors.New("boom")
		},
	}

	ctx := context.Background()
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "完整回复"})); err != nil {
		t.Fatalf("push delta: %v", err)
	}
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventFinal, Final: &channel.StreamFinalizePayload{}})); err != nil {
		t.Fatalf("push final: %v", err)
	}

	if len(sent) != 1 {
		t.Fatalf("expected one regular send, got %d", len(sent))
	}
	if got := sent[0].Message.PlainText(); got != "完整回复" {
		t.Fatalf("unexpected text: %q", got)
	}
}

func TestQQPreviewC2CSingleShardForShortReply(t *testing.T) {
	t.Parallel()

	var shards []qqStreamShardRequest
	stream := &qqPreview{
		target:         "c2c:user-openid",
		streamInterval: time.Hour,
		now:            time.Now,
		send: func(context.Context, channel.PreparedOutboundMessage) error {
			t.Fatal("regular send must not run")
			return nil
		},
		streamSend: func(_ context.Context, req qqStreamShardRequest) (qqStreamShardResponse, error) {
			shards = append(shards, req)
			return qqStreamShardResponse{ID: "sm-9"}, nil
		},
	}

	ctx := context.Background()
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "短回复"})); err != nil {
		t.Fatalf("push delta: %v", err)
	}
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventFinal, Final: &channel.StreamFinalizePayload{}})); err != nil {
		t.Fatalf("push final: %v", err)
	}

	if len(shards) != 2 {
		t.Fatalf("expected 2 shards (immediate + final), got %d", len(shards))
	}
	if shards[0].InputState != qqStreamInputGenerating || shards[0].ContentRaw != "短回复" {
		t.Fatalf("unexpected first shard: %+v", shards[0])
	}
	if shards[1].InputState != qqStreamInputDone || shards[1].ContentRaw != "短回复" {
		t.Fatalf("unexpected final shard: %+v", shards[1])
	}
}

func TestQQPreviewC2CThrottlesMiddleShards(t *testing.T) {
	t.Parallel()

	var shards []qqStreamShardRequest
	base := time.Now()
	current := base
	stream := &qqPreview{
		target:         "c2c:user-openid",
		streamInterval: time.Second,
		now:            func() time.Time { return current },
		send: func(context.Context, channel.PreparedOutboundMessage) error {
			t.Fatal("regular send must not run")
			return nil
		},
		streamSend: func(_ context.Context, req qqStreamShardRequest) (qqStreamShardResponse, error) {
			shards = append(shards, req)
			return qqStreamShardResponse{ID: "sm-1"}, nil
		},
	}

	ctx := context.Background()
	push := func(delta string) {
		t.Helper()
		if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: delta})); err != nil {
			t.Fatalf("push delta: %v", err)
		}
	}
	push("一")
	push("二") // within interval: dropped
	current = base.Add(2 * time.Second)
	push("三")
	push("四") // within interval: dropped
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventFinal, Final: &channel.StreamFinalizePayload{}})); err != nil {
		t.Fatalf("push final: %v", err)
	}

	if len(shards) != 3 {
		t.Fatalf("expected 3 shards, got %d", len(shards))
	}
	if shards[1].ContentRaw != "一二三" {
		t.Fatalf("second shard must carry cumulative text: %q", shards[1].ContentRaw)
	}
	if shards[2].InputState != qqStreamInputDone || shards[2].ContentRaw != "一二三四" {
		t.Fatalf("final shard must complete full text: %+v", shards[2])
	}
}

func TestQQPreviewAbortRetiresUnfinishedStream(t *testing.T) {
	t.Parallel()

	var shards []qqStreamShardRequest
	stream := &qqPreview{
		target:         "c2c:user-openid",
		streamInterval: 0,
		now:            time.Now,
		send: func(context.Context, channel.PreparedOutboundMessage) error {
			t.Fatal("regular send must not run for a retired stream")
			return nil
		},
		streamSend: func(_ context.Context, req qqStreamShardRequest) (qqStreamShardResponse, error) {
			shards = append(shards, req)
			return qqStreamShardResponse{ID: "sm-1"}, nil
		},
	}

	// A turn that dies before StreamEventFinal: its context is already gone
	// by the time Abort runs, which is exactly when the client would be left
	// rendering a permanently unfinished message.
	ctx, cancel := context.WithCancel(context.Background())
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "半截回复"})); err != nil {
		t.Fatalf("push delta: %v", err)
	}
	cancel()
	if err := stream.Abort(ctx); err != nil {
		t.Fatalf("Abort must not fail on a canceled context: %v", err)
	}

	if len(shards) != 2 {
		t.Fatalf("expected the delta shard plus a closing shard, got %d: %+v", len(shards), shards)
	}
	closing := shards[1]
	if closing.InputState != qqStreamInputDone {
		t.Fatalf("closing shard state = %d, want %d", closing.InputState, qqStreamInputDone)
	}
	if closing.ContentRaw != "半截回复" {
		t.Fatalf("closing shard must replay the last accepted content, got %q", closing.ContentRaw)
	}
	if closing.StreamMsgID != "sm-1" {
		t.Fatalf("closing shard must reuse the server stream id: %+v", closing)
	}
}

func TestQQPreviewAbortSkipsRetireAfterNormalFinal(t *testing.T) {
	t.Parallel()

	var shards []qqStreamShardRequest
	stream := &qqPreview{
		target:         "c2c:user-openid",
		streamInterval: 0,
		now:            time.Now,
		send:           func(context.Context, channel.PreparedOutboundMessage) error { return nil },
		streamSend: func(_ context.Context, req qqStreamShardRequest) (qqStreamShardResponse, error) {
			shards = append(shards, req)
			return qqStreamShardResponse{ID: "sm-1"}, nil
		},
	}

	ctx := context.Background()
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "完整回复"})); err != nil {
		t.Fatalf("push delta: %v", err)
	}
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventFinal, Final: &channel.StreamFinalizePayload{}})); err != nil {
		t.Fatalf("push final: %v", err)
	}
	before := len(shards)
	if err := stream.Abort(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(shards) != before {
		t.Fatalf("a completed stream must not be retired again: %+v", shards[before:])
	}
}

func TestQQPreviewFinalShardFailureRetiresStreamBeforeFallback(t *testing.T) {
	t.Parallel()

	var shards []qqStreamShardRequest
	var sent []channel.OutboundMessage
	finalFailed := false
	stream := &qqPreview{
		target:         "c2c:user-openid",
		streamInterval: 0,
		now:            time.Now,
		send: func(_ context.Context, msg channel.PreparedOutboundMessage) error {
			sent = append(sent, msg.LogicalMessage())
			return nil
		},
		streamSend: func(_ context.Context, req qqStreamShardRequest) (qqStreamShardResponse, error) {
			// The first attempt to close the stream fails, mirroring a
			// timeout on the last call of an otherwise healthy stream; the
			// retire shard that follows must still land.
			if req.InputState == qqStreamInputDone && !finalFailed {
				finalFailed = true
				return qqStreamShardResponse{}, errors.New("boom")
			}
			shards = append(shards, req)
			return qqStreamShardResponse{ID: "sm-1"}, nil
		},
	}

	ctx := context.Background()
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "半截回复"})); err != nil {
		t.Fatalf("push delta: %v", err)
	}
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "，完整结尾"})); err != nil {
		t.Fatalf("push delta: %v", err)
	}
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventFinal, Final: &channel.StreamFinalizePayload{}})); err != nil {
		t.Fatalf("push final: %v", err)
	}

	last := shards[len(shards)-1]
	if last.InputState != qqStreamInputDone {
		t.Fatalf("stream must be retired after a failed final shard, got %+v", shards)
	}
	if last.ContentRaw != "半截回复，完整结尾" {
		t.Fatalf("retire shard must replay the last accepted content, got %q", last.ContentRaw)
	}
	if len(sent) != 1 || sent[0].Message.PlainText() != "半截回复，完整结尾" {
		t.Fatalf("fallback must still carry the complete text, got %+v", sent)
	}
}

func TestQQPreviewC2CResetRetiresStreamAndSendsRegeneratedReply(t *testing.T) {
	t.Parallel()

	var shards []qqStreamShardRequest
	var sent []channel.OutboundMessage
	stream := &qqPreview{
		target:         "c2c:user-openid",
		streamInterval: 0,
		now:            time.Now,
		send: func(_ context.Context, msg channel.PreparedOutboundMessage) error {
			sent = append(sent, msg.LogicalMessage())
			return nil
		},
		streamSend: func(_ context.Context, req qqStreamShardRequest) (qqStreamShardResponse, error) {
			shards = append(shards, req)
			return qqStreamShardResponse{ID: "sm-1"}, nil
		},
	}

	ctx := context.Background()
	events := []channel.StreamEvent{
		{Type: channel.StreamEventDelta, Delta: "失败的半截"},
		{Type: channel.StreamEventReset},
		{Type: channel.StreamEventDelta, Delta: "重试后的回复"},
		{Type: channel.StreamEventFinal, Final: &channel.StreamFinalizePayload{}},
	}
	for _, event := range events {
		if err := stream.drive(ctx, preparedQQEvent(event)); err != nil {
			t.Fatalf("push %s: %v", event.Type, err)
		}
	}
	if err := stream.Abort(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}

	if len(shards) != 2 {
		t.Fatalf("expected the failed attempt's shard plus its closing shard, got %d: %+v", len(shards), shards)
	}
	if closing := shards[1]; closing.InputState != qqStreamInputDone || closing.ContentRaw != "失败的半截" {
		t.Fatalf("reset must retire the stream at its last content, got %+v", closing)
	}
	if len(sent) != 1 {
		t.Fatalf("expected one regular send for the regenerated reply, got %d", len(sent))
	}
	if got := sent[0].Message.PlainText(); got != "重试后的回复" {
		t.Fatalf("regenerated reply = %q, want only the surviving attempt's text", got)
	}
}

// Finish with no final is a run that said nothing: the text held back is not
// sent, and a stream that is open is retired where it stands.
func TestQQPreviewFinishWithNoFinalSendsNothing(t *testing.T) {
	t.Parallel()

	t.Run("text held until the end", func(t *testing.T) {
		t.Parallel()
		stream := &qqPreview{
			target: "group:group-openid",
			send: func(context.Context, channel.PreparedOutboundMessage) error {
				t.Fatal("a reply that said nothing must not send")
				return nil
			},
		}
		ctx := context.Background()
		if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "NO_REPLY"})); err != nil {
			t.Fatalf("push delta: %v", err)
		}
		if err := stream.Finish(ctx, nil); err != nil {
			t.Fatalf("finish: %v", err)
		}
	})

	t.Run("open stream", func(t *testing.T) {
		t.Parallel()
		var shards []qqStreamShardRequest
		stream := &qqPreview{
			target:         "c2c:user-openid",
			streamInterval: 0,
			now:            time.Now,
			send: func(context.Context, channel.PreparedOutboundMessage) error {
				t.Fatal("a reply that said nothing must not send")
				return nil
			},
			streamSend: func(_ context.Context, req qqStreamShardRequest) (qqStreamShardResponse, error) {
				shards = append(shards, req)
				return qqStreamShardResponse{ID: "sm-1"}, nil
			},
		}
		ctx := context.Background()
		if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "NO_REPLY"})); err != nil {
			t.Fatalf("push delta: %v", err)
		}
		if err := stream.Finish(ctx, nil); err != nil {
			t.Fatalf("finish: %v", err)
		}
		if len(shards) != 2 || shards[1].InputState != qqStreamInputDone || shards[1].ContentRaw != "NO_REPLY" {
			t.Fatalf("shards = %+v, want the open stream retired at its last content", shards)
		}
	})
}

// A tool that finishes while the reply is still being produced is reported at
// once; the text waits for the end.
func TestQQPreviewSendsAToolResultWhenItFinishes(t *testing.T) {
	t.Parallel()

	var sent []channel.OutboundMessage
	stream := &qqPreview{
		target: "group:group-openid",
		reply:  &channel.ReplyRef{MessageID: "src-1"},
		send: func(_ context.Context, msg channel.PreparedOutboundMessage) error {
			sent = append(sent, msg.LogicalMessage())
			return nil
		},
	}
	if err := stream.Update(context.Background(), channel.PreparedStreamEvent{
		Type:     channel.StreamEventToolCallEnd,
		ToolCall: &channel.StreamToolCall{Name: "search", CallID: "call-1"},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(sent) != 1 || !strings.Contains(sent[0].Message.PlainText(), "search") {
		t.Fatalf("sent = %+v, want one message naming the tool", sent)
	}
	if sent[0].Message.Reply == nil || sent[0].Message.Reply.MessageID != "src-1" {
		t.Fatalf("the tool result must reply to the source message: %+v", sent[0].Message.Reply)
	}
}

// Only a direct chat with streaming enabled streams; the rest hold the text.
func TestOpenPreviewStreamsOnlyInADirectChat(t *testing.T) {
	t.Parallel()

	creds := func(streaming bool) map[string]any {
		return map[string]any{"appId": "app-1", "clientSecret": "secret-1", "markdownSupport": true, "enableStreaming": streaming}
	}
	cases := []struct {
		name       string
		streaming  bool
		target     string
		wantStream bool
	}{
		{"direct chat", true, "c2c:user-openid", true},
		{"direct chat, streaming off", false, "c2c:user-openid", false},
		{"group", true, "group:group-openid", false},
		{"channel", true, "channel:12345", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			preview, err := NewQQAdapter(nil).OpenPreview(context.Background(),
				channel.ChannelConfig{ID: "cfg-1", Credentials: creds(tc.streaming)}, tc.target,
				channel.StreamOptions{Reply: &channel.ReplyRef{MessageID: "src-1"}})
			if err != nil {
				t.Fatalf("OpenPreview: %v", err)
			}
			got, ok := preview.(*qqPreview)
			if !ok {
				t.Fatalf("preview is %T, want *qqPreview", preview)
			}
			if (got.streamSend != nil) != tc.wantStream {
				t.Fatalf("streams = %v, want %v", got.streamSend != nil, tc.wantStream)
			}
		})
	}
}

// The reply window is the one QQ documents for each kind of conversation, and
// a message sent after it is an active message.
func TestReplyWindowFollowsTheConversation(t *testing.T) {
	t.Parallel()

	adapter := NewQQAdapter(nil)
	base := adapter.Descriptor().Capabilities
	if base.Preview != channel.PreviewNativeStream || !base.ProactiveSend {
		t.Fatalf("capabilities = %+v, want a native stream that can send proactively", base)
	}
	cfg := channel.ChannelConfig{Credentials: map[string]any{"appId": "app-1", "clientSecret": "secret-1"}}
	cases := map[string]time.Duration{
		"c2c:user-openid":    60 * time.Minute,
		"group:group-openid": 5 * time.Minute,
		"channel:12345":      5 * time.Minute,
	}
	for target, want := range cases {
		if got := adapter.ResolveOutboundCapabilities(cfg, target, base).ReplyWindow; got != want {
			t.Errorf("%s: ReplyWindow = %v, want %v", target, got, want)
		}
	}
}

// A final that arrives after the turn's context is gone still retires the
// stream: nothing calls Abort once Finish has run.
func TestQQPreviewFinishOnCanceledContextRetiresTheStream(t *testing.T) {
	t.Parallel()

	var shards []qqStreamShardRequest
	stream := &qqPreview{
		target:         "c2c:user-openid",
		streamInterval: 0,
		now:            time.Now,
		send:           func(context.Context, channel.PreparedOutboundMessage) error { return context.Canceled },
		streamSend: func(ctx context.Context, req qqStreamShardRequest) (qqStreamShardResponse, error) {
			if ctx.Err() != nil {
				return qqStreamShardResponse{}, ctx.Err()
			}
			shards = append(shards, req)
			return qqStreamShardResponse{ID: "sm-1"}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "半截回复"})); err != nil {
		t.Fatalf("push delta: %v", err)
	}
	cancel()
	_ = stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventFinal, Final: &channel.StreamFinalizePayload{}}))

	if last := shards[len(shards)-1]; last.InputState != qqStreamInputDone || last.ContentRaw != "半截回复" {
		t.Fatalf("the stream must be retired at its last content, got %+v", shards)
	}
}

func TestQQOutboundStreamShardFailureIsRecordedAsEventPerAttempt(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	stream := &qqPreview{
		target:         "c2c:user-openid",
		streamInterval: 0,
		now:            time.Now,
		logger:         slog.New(slog.NewJSONHandler(&buf, nil)),
		send:           func(context.Context, channel.PreparedOutboundMessage) error { return nil },
		streamSend: func(context.Context, qqStreamShardRequest) (qqStreamShardResponse, error) {
			return qqStreamShardResponse{}, errors.New("boom")
		},
	}

	ctx := context.Background()
	if err := stream.drive(ctx, preparedQQEvent(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "reply"})); err != nil {
		t.Fatalf("push delta: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("records after one failed shard = %d, want 1: %s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], `"level":"WARN"`) || !strings.Contains(lines[0], `"fault"`) {
		t.Fatalf("record = %s, want a WARN event with fault", lines[0])
	}
}
