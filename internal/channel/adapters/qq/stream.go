package qq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/errlog"
	"github.com/felinics/memoh/internal/errs"
	"github.com/felinics/memoh/internal/redact"
)

// defaultStreamShardInterval throttles C2C stream shards; QQ allows 50 QPS
// on the endpoint, but the client-side typewriter reads better at ~1/s and
// fewer shards means fewer chances for the known middle-shard drops.
const defaultStreamShardInterval = time.Second

// streamCloseTimeout bounds the best-effort closing shard sent when a turn is
// aborted or the final shard fails.
const streamCloseTimeout = 5 * time.Second

// qqPreview shows one reply while it is produced. A direct chat with streaming
// enabled shows it through QQ's stream messages; every other target holds the
// text until the reply ends. Results of tools that finish in between are sent
// as they come.
type qqPreview struct {
	target string
	reply  *channel.ReplyRef
	send   func(context.Context, channel.PreparedOutboundMessage) error

	// C2C streaming via /v2/users/{openid}/stream_messages. A nil streamSend
	// keeps the buffer-till-final behavior (group/channel targets, streaming
	// disabled, or no passive-reply anchor message).
	streamSend     func(context.Context, qqStreamShardRequest) (qqStreamShardResponse, error)
	streamInterval time.Duration
	now            func() time.Time
	logger         *slog.Logger

	closed atomic.Bool
	mu     sync.Mutex
	buffer strings.Builder

	streamMsgID  string
	streamIndex  int
	lastShardAt  time.Time
	streamBroken bool
	// lastShardContent is the newest content QQ accepted; retiring the
	// stream has to replay it because replace mode rejects any payload that
	// is not prefixed by what was already delivered.
	lastShardContent string
	streamDone       bool
}

var _ channel.PreviewOpener = (*QQAdapter)(nil)

// OpenPreview opens the preview of a reply.
func (a *QQAdapter) OpenPreview(_ context.Context, cfg channel.ChannelConfig, target string, opts channel.StreamOptions) (channel.PreparedPreview, error) {
	parsed, err := parseConfig(cfg.Credentials)
	if err != nil {
		return nil, fmt.Errorf("qq open preview: %w", err)
	}
	redact.SetSecrets("qq:"+parsed.AppID, parsed.AppSecret)
	preview := &qqPreview{
		target: target,
		reply:  opts.Reply,
		send: func(ctx context.Context, msg channel.PreparedOutboundMessage) error {
			if msg.Target == "" {
				msg.Target = target
			}
			if msg.Message.Message.Reply == nil && opts.Reply != nil {
				msg.Message.Message.Reply = opts.Reply
			}
			return a.Send(ctx, cfg, msg)
		},
		streamInterval: defaultStreamShardInterval,
		now:            time.Now,
		logger:         a.logger,
	}
	// Streamed shards are markdown payloads (QQ markdown messages), so the
	// C2C stream is only wired when markdown rendering is enabled; otherwise
	// the buffered path keeps its inline-markup stripping.
	if parsed.EnableStreaming && parsed.MarkdownSupport {
		if parsedTarget, targetErr := parseTarget(target); targetErr == nil && parsedTarget.Kind == qqTargetC2C {
			if replyTo := streamReplyMessageID(opts); replyTo != "" {
				client := a.getOrCreateClient(cfg, parsed)
				preview.streamSend = func(ctx context.Context, req qqStreamShardRequest) (qqStreamShardResponse, error) {
					return client.sendStreamShard(ctx, parsedTarget.ID, replyTo, req)
				}
			}
		}
	}
	return preview, nil
}

func streamReplyMessageID(opts channel.StreamOptions) string {
	if opts.Reply != nil && strings.TrimSpace(opts.Reply.MessageID) != "" {
		return strings.TrimSpace(opts.Reply.MessageID)
	}
	return strings.TrimSpace(opts.SourceMessageID)
}

// Update shows an event of the reply. Attachments never reach it: the mapping
// layer sends them as messages of their own.
func (s *qqPreview) Update(ctx context.Context, event channel.PreparedStreamEvent) error {
	if s == nil || s.send == nil {
		return errors.New("qq preview not configured")
	}
	if s.closed.Load() {
		return errors.New("qq preview is closed")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	switch event.Type {
	case channel.StreamEventToolCallEnd:
		text := strings.TrimSpace(channel.RenderToolCallMessage(channel.BuildToolCallEnd(event.ToolCall)))
		if text == "" {
			return nil
		}
		return s.send(ctx, channel.PreparedOutboundMessage{
			Target: s.target,
			Message: channel.PreparedMessage{
				Message: channel.Message{Format: channel.MessageFormatPlain, Text: text, Reply: s.reply},
			},
		})
	case channel.StreamEventDelta:
		if event.Phase == channel.StreamPhaseReasoning || event.Delta == "" {
			return nil
		}
		s.mu.Lock()
		s.buffer.WriteString(event.Delta)
		content := s.buffer.String()
		due := s.streamSend != nil && !s.streamBroken && s.now().Sub(s.lastShardAt) >= s.streamInterval
		s.mu.Unlock()
		if !due {
			return nil
		}
		// Shard failures must not fail the turn: the stream is marked broken
		// and the final flush falls back to a regular message.
		_ = s.pushShard(ctx, qqStreamInputGenerating, content)
		return nil
	case channel.StreamEventReset:
		s.mu.Lock()
		s.buffer.Reset()
		opened := s.streamSend != nil && s.streamIndex > 0
		s.mu.Unlock()
		if opened {
			// Replace mode only accepts content prefixed by what QQ already
			// shows, so the regenerated reply cannot continue this stream.
			// Retire it where it stands and deliver the reply through the
			// buffer-till-final path.
			s.finishStream(context.WithoutCancel(ctx))
			s.mu.Lock()
			s.streamSend = nil
			s.mu.Unlock()
		}
		return nil
	case channel.StreamEventError:
		// Nothing has been shown that the error would overwrite, so it is the
		// reply's message.
		errText := strings.TrimSpace(event.Error)
		if errText == "" {
			return nil
		}
		return s.sendMessage(ctx, channel.PreparedMessage{
			Message: channel.Message{Text: channel.ErrorReplyText(event.ErrorCode, errText)},
		})
	default:
		return nil
	}
}

// Finish ends the reply, once. A nil final means the run said nothing: text
// that was held back is dropped and a stream that is open is retired.
func (s *qqPreview) Finish(ctx context.Context, final *channel.PreparedStreamEvent) error {
	if s == nil || s.send == nil {
		return errors.New("qq preview not configured")
	}
	// A canceled ctx does not return early: Abort is not called after Finish, so
	// an open stream is retired here, by flush when its final shard fails.
	if s.closed.Swap(true) {
		return nil
	}
	if final == nil {
		s.finishStream(context.WithoutCancel(ctx))
		return nil
	}
	var msg channel.PreparedMessage
	if final.Final != nil {
		msg = final.Final.Message
	}
	return s.flush(ctx, msg)
}

// Abort ends the reply that has no final. A stream that is open is retired so
// QQ stops rendering it as unfinished; nothing is added to it.
func (s *qqPreview) Abort(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if s.closed.Swap(true) {
		return nil
	}
	// The turn's context is usually already canceled by now, so the closing
	// shard gets a cancellation-free context of its own.
	s.finishStream(context.WithoutCancel(ctx))
	return nil
}

// pushShard sends one replace-mode shard carrying the full text so far.
// Cumulative content keeps the server-side prefix valid even when QQ drops
// middle shards, so a stream can always be completed by the final shard.
func (s *qqPreview) pushShard(ctx context.Context, state int, content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	s.mu.Lock()
	if s.streamSend == nil {
		s.mu.Unlock()
		return errors.New("qq stream shards not configured")
	}
	req := qqStreamShardRequest{
		StreamMsgID: s.streamMsgID,
		Index:       s.streamIndex,
		InputState:  state,
		ContentRaw:  content,
	}
	s.mu.Unlock()

	resp, err := s.streamSend(ctx, req)
	if err != nil {
		s.mu.Lock()
		s.streamBroken = true
		logger := s.logger
		s.mu.Unlock()
		// Every caller drops the error and falls back to a regular message, so
		// this event is the shard failure's only record.
		if logger != nil {
			result := errlog.Event(ctx, "channel.qq.stream", errs.Wrap(err, "send qq stream shard", slog.String("target", s.target), slog.Int("index", req.Index)), errlog.Options{})
			logger.LogAttrs(ctx, result.Level, "qq stream shard failed", result.Attrs()...)
		}
		return err
	}
	s.mu.Lock()
	if strings.TrimSpace(resp.ID) != "" {
		s.streamMsgID = strings.TrimSpace(resp.ID)
	}
	s.streamIndex++
	s.lastShardAt = s.now()
	s.streamBroken = false
	s.lastShardContent = content
	if state == qqStreamInputDone {
		s.streamDone = true
	}
	s.mu.Unlock()
	return nil
}

// finishStream retires an open stream with input_state=10 carrying the last
// content QQ accepted. Without it the QQ client renders the message as
// forever "generating" — an unfinished stream never times out on its own.
func (s *qqPreview) finishStream(ctx context.Context) {
	s.mu.Lock()
	content := s.lastShardContent
	pending := s.streamSend != nil && s.streamIndex > 0 && !s.streamDone && content != ""
	s.mu.Unlock()
	if !pending {
		return
	}
	closeCtx, cancel := context.WithTimeout(ctx, streamCloseTimeout)
	defer cancel()
	_ = s.pushShard(closeCtx, qqStreamInputDone, content)
}

// flush ends the reply with msg. The text the preview held back or streamed is
// what the user was shown, so it is the text of the end.
func (s *qqPreview) flush(ctx context.Context, msg channel.PreparedMessage) error {
	s.mu.Lock()
	bufferedText := strings.TrimSpace(s.buffer.String())
	canStream := s.streamSend != nil
	s.buffer.Reset()
	s.mu.Unlock()

	logicalMsg := msg.LogicalMessage()
	if bufferedText != "" {
		logicalMsg.Text = bufferedText
		logicalMsg.Parts = nil
		if logicalMsg.Format == "" {
			logicalMsg.Format = channel.MessageFormatPlain
		}
	}

	text := strings.TrimSpace(logicalMsg.PlainText())
	if canStream && text != "" {
		// The final shard completes the stream with the full text; replace
		// mode heals any middle shards QQ dropped. Only a failed final shard
		// falls back to a regular message.
		if err := s.pushShard(ctx, qqStreamInputDone, text); err == nil {
			logicalMsg.Text = ""
			logicalMsg.Parts = nil
			logicalMsg.Format = ""
		} else {
			// Earlier shards already put partial text on screen. Retire the
			// stream at that point so it stops rendering as unfinished; the
			// fallback below still carries the complete text.
			s.finishStream(context.WithoutCancel(ctx))
		}
	}
	return s.sendMessage(ctx, channel.PreparedMessage{Message: logicalMsg, Attachments: msg.Attachments})
}

// sendMessage sends msg to the reply's target, as a reply to the message the
// reply answers unless msg names another. An empty message is not sent.
func (s *qqPreview) sendMessage(ctx context.Context, msg channel.PreparedMessage) error {
	if msg.Message.Reply == nil && s.reply != nil {
		msg.Message.Reply = s.reply
	}
	if msg.Message.IsEmpty() && len(msg.Attachments) == 0 {
		return nil
	}
	return s.send(ctx, channel.PreparedOutboundMessage{Target: s.target, Message: msg})
}
