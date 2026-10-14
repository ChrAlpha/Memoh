package delivery

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/i18n"
)

// Config is what the caller knows when a reply starts.
type Config struct {
	Surface Surface
	Target  string
	// Reply, SourceMessageID and Metadata open the first preview.
	Reply           *channel.ReplyRef
	SourceMessageID string
	Metadata        map[string]any
	// ReceivedAt is when the inbound message arrived.
	ReceivedAt time.Time
	// HideToolCalls keeps ordinary tool events from the preview.
	HideToolCalls bool
	// Observe receives a copy of each event the reply shows, for subscribers
	// that mirror the conversation.
	Observe func(ctx context.Context, event channel.StreamEvent)
	// Now returns the time of a decision; time.Now when nil.
	Now func() time.Time
	// Logger and LogAttrs report what the reply skips: an event that cannot be
	// decoded and a reaction the platform refuses.
	Logger   *slog.Logger
	LogAttrs []slog.Attr
}

// Reply is one run's reply to a platform conversation. It owns the preview and
// guarantees it ends once.
type Reply struct {
	cfg     Config
	caps    channel.ChannelCapabilities
	state   State
	preview channel.Preview
}

// Open starts a reply: it reads the channel's capabilities for the target and
// opens the preview unless the channel shows none.
func Open(ctx context.Context, cfg Config) (*Reply, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	r := &Reply{
		cfg:  cfg,
		caps: cfg.Surface.Capabilities(cfg.Target),
		state: State{
			Target:          cfg.Target,
			Reply:           cfg.Reply,
			SourceMessageID: cfg.SourceMessageID,
			ReceivedAt:      cfg.ReceivedAt,
			HideToolCalls:   cfg.HideToolCalls,
		},
	}
	if r.caps.Preview != channel.PreviewNone {
		// The preview quotes the source message by the window's rule at the moment
		// it opens. A window that closes while the preview is open does not change
		// the reference the preview already carries; what the reply sends after
		// that follows Plan.
		st := r.state
		st.Now = cfg.Now()
		reach, st := windowOK(r.caps, st)
		if !reach {
			return nil, apperror.Wrap(apperror.CodeChannelReplyWindowExpired, errReplyWindowExpired, nil)
		}
		preview, err := cfg.Surface.OpenPreview(ctx, cfg.Target, channel.StreamOptions{
			Reply:           st.replyRef(cfg.Reply),
			SourceMessageID: cfg.SourceMessageID,
			Metadata:        cfg.Metadata,
		})
		if err != nil {
			return nil, err
		}
		r.preview = preview
	}
	return r, nil
}

// Start shows that the run has begun.
func (r *Reply) Start(ctx context.Context) error {
	return r.perform(ctx, []Action{{Kind: KindUpdate, Event: channel.StreamEvent{
		Type:   channel.StreamEventStatus,
		Status: channel.StreamStatusStarted,
	}}})
}

// Fail shows an error for a run that did not start. It is best effort: the
// run's own error is what the caller returns.
func (r *Reply) Fail(ctx context.Context, event channel.StreamEvent) {
	_ = r.Show(ctx, event)
}

// Close ends the preview if the reply has not. The preview's end does not
// follow a canceled ctx, so it is never left open.
func (r *Reply) Close(ctx context.Context) error {
	return r.abort(ctx)
}

// RunInput is what one run's reply needs from its caller.
type RunInput struct {
	// ReportsTerminal says the run ends with a run_terminal event that names
	// its failure code.
	ReportsTerminal bool
	Localizer       *i18n.Localizer
	// IngestAttachments stores the attachments of an event and returns the
	// ones to show.
	IngestAttachments func(ctx context.Context, attachments []channel.Attachment) []channel.Attachment
	// Outputs turns the run's final model messages into the messages to send,
	// and reports whether a messaging tool already replied.
	Outputs func(messages []turn.ModelMessage) ([]channel.Message, bool)
	// StreamFailure renders the failure reply for a run error that no
	// run_terminal event answered.
	StreamFailure func(err error) (channel.StreamEvent, bool)
}

// Run consumes one run's events until both channels close, and performs the
// actions Plan returns for each. It returns the run's error, or the error of
// the write that refused. The preview is always ended: finished when the run
// ends normally, aborted when it fails.
func (r *Reply) Run(ctx context.Context, in RunInput, events <-chan turn.Event, errs <-chan error) error {
	failures := runFailureReply{reportsTerminal: in.ReportsTerminal}
	r.state.HoldErrors = in.ReportsTerminal

	var (
		finalMessages []turn.ModelMessage
		runErr        error
		writeErr      error
	)
	for events != nil || errs != nil {
		select {
		case turnEvent, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if failures.observeTerminal(turnEvent) {
				continue
			}
			decoded, messages, err := Decode(turnEvent.Payload, in.Localizer)
			if err != nil {
				r.warnChunk(ctx, err)
				continue
			}
			for i := range decoded {
				prepare(ctx, in, &decoded[i])
				if err := r.Show(ctx, decoded[i]); err != nil {
					writeErr = err
					break
				}
			}
			if writeErr != nil {
				break
			}
			if len(messages) > 0 {
				finalMessages = messages
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			// A run error does not end consumption: events the run produced before
			// it are still delivered, and the event channel closes after them.
			if err != nil && runErr == nil {
				runErr = err
			}
		}
		if writeErr != nil {
			break
		}
	}
	streamErr := runErr
	if writeErr != nil {
		streamErr = writeErr
	}
	return r.finish(ctx, in, &failures, finalMessages, streamErr)
}

// prepare gives an event what the preview needs before it is planned: the
// locale of a user-input prompt and the stored form of its attachments.
func prepare(ctx context.Context, in RunInput, ev *channel.StreamEvent) {
	if ev.Type == channel.StreamEventToolCallStart && ev.ToolCall != nil && channel.HasUserInputAction(ev.ToolCall.Actions) {
		ev.ToolCall.Locale = in.Localizer.Locale()
	}
	if ev.Type == channel.StreamEventAttachment && len(ev.Attachments) > 0 && in.IngestAttachments != nil {
		ev.Attachments = in.IngestAttachments(ctx, ev.Attachments)
	}
}

// finish ends the run: the release of held errors and the final messages when
// it succeeded, the failure reply and an abort when it did not.
func (r *Reply) finish(ctx context.Context, in RunInput, failures *runFailureReply, finalMessages []turn.ModelMessage, streamErr error) error {
	r.state.Now = r.cfg.Now()
	st := r.state
	if failures.terminal != nil {
		st.held = nil
	}
	if streamErr != nil {
		if ev, ok := failures.reply(in.Localizer, streamErr); ok {
			st = SetFailure(st, ev)
		} else if ev, ok := in.StreamFailure(streamErr); ok {
			st = SetFailure(st, ev)
		}
		actions, _ := PlanErrors(r.caps, st)
		cctx := context.WithoutCancel(ctx)
		// The preview's buffered text is flushed before a new error message is
		// sent, so the error follows the text it failed.
		if st.streamed {
			_ = r.abort(cctx)
			_ = r.perform(cctx, actions)
		} else {
			_ = r.perform(cctx, actions)
			_ = r.abort(cctx)
		}
		return streamErr
	}
	if ev, ok := failures.reply(in.Localizer, nil); ok {
		st = SetFailure(st, ev)
	}
	r.state = st
	outputs, suppress := in.Outputs(finalMessages)
	return r.End(ctx, outputs, suppress)
}

// End ends a run that succeeded: the errors it still owes, then its messages.
// The first message ends the preview and each later one is a message of its
// own. suppress marks a run whose replies a messaging tool already sent.
func (r *Reply) End(ctx context.Context, outputs []channel.Message, suppress bool) error {
	r.state.Now = r.cfg.Now()
	actions, next := PlanEnd(r.caps, r.state, outputs, suppress)
	r.state = next
	if err := r.perform(ctx, actions); err != nil {
		return err
	}
	r.observe(ctx, channel.StreamEvent{Type: channel.StreamEventStatus, Status: channel.StreamStatusCompleted})
	return nil
}

// Show decides one event and performs what it calls for.
func (r *Reply) Show(ctx context.Context, ev channel.StreamEvent) error {
	r.state.Now = r.cfg.Now()
	actions, next := Plan(ev, r.caps, r.state)
	r.state = next
	return r.perform(ctx, actions)
}

// perform runs actions in order and stops at the first that fails.
func (r *Reply) perform(ctx context.Context, actions []Action) error {
	for _, action := range actions {
		if err := r.perform1(ctx, action); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reply) perform1(ctx context.Context, action Action) error {
	switch action.Kind {
	case KindUpdate:
		var err error
		if r.preview != nil {
			err = r.preview.Update(ctx, action.Event)
		}
		r.observe(ctx, action.Event)
		return err
	case KindObserve:
		r.observe(ctx, action.Event)
	case KindEndPreview:
		return r.endPreview(ctx, action.Final)
	case KindOpenPreview:
		return r.reopen(ctx)
	case KindFinish:
		err := r.endPreview(ctx, action.Final)
		if action.Final != nil && !action.Final.Final.Message.IsEmpty() {
			r.observe(ctx, *action.Final)
		}
		return err
	case KindSend:
		err := r.cfg.Surface.Send(ctx, action.Message)
		if action.Event.Type != "" {
			r.observe(ctx, action.Event)
		}
		return err
	case KindAttach:
		var err error
		for _, att := range action.Attachments {
			if err = r.cfg.Surface.Attach(ctx, r.cfg.Target, att, action.Reply); err != nil {
				break
			}
		}
		r.observe(ctx, action.Event)
		return err
	case KindReact:
		for _, req := range action.Reactions {
			if err := r.cfg.Surface.React(ctx, req); err != nil {
				r.warnReaction(ctx, req, err)
			}
		}
	case KindFail:
		return apperror.Wrap(action.Code, errReplyWindowExpired, nil)
	}
	return nil
}

// endPreview finishes the open preview, if there is one.
func (r *Reply) endPreview(ctx context.Context, final *channel.StreamEvent) error {
	preview := r.preview
	r.preview = nil
	if preview == nil {
		return nil
	}
	return preview.Finish(ctx, final)
}

// abort aborts the open preview, if there is one.
func (r *Reply) abort(ctx context.Context) error {
	preview := r.preview
	r.preview = nil
	if preview == nil {
		return nil
	}
	return preview.Abort(context.WithoutCancel(ctx))
}

// reopen opens the preview that carries on after the previous one ended. It
// does not quote the source message again.
func (r *Reply) reopen(ctx context.Context) error {
	preview, err := r.cfg.Surface.OpenPreview(ctx, r.cfg.Target, channel.StreamOptions{
		SourceMessageID: r.cfg.SourceMessageID,
		Metadata:        r.cfg.Metadata,
		Continuation:    true,
	})
	if err != nil {
		return err
	}
	r.preview = preview
	return preview.Update(ctx, channel.StreamEvent{Type: channel.StreamEventStatus, Status: channel.StreamStatusStarted})
}

func (r *Reply) observe(ctx context.Context, event channel.StreamEvent) {
	if r.cfg.Observe != nil && event.Type != "" {
		r.cfg.Observe(ctx, event)
	}
}

func (r *Reply) warnChunk(ctx context.Context, err error) {
	if r.cfg.Logger == nil {
		return
	}
	r.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "stream chunk parse failed", r.logAttrs(slog.Any("error", err))...)
}

func (r *Reply) warnReaction(ctx context.Context, req channel.ReactRequest, err error) {
	if r.cfg.Logger == nil {
		return
	}
	r.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "inline reaction failed", r.logAttrs(
		slog.String("emoji", req.Emoji), slog.String("message_id", req.MessageID), slog.Any("error", err))...)
}

func (r *Reply) logAttrs(attrs ...slog.Attr) []slog.Attr {
	return append(append([]slog.Attr{}, r.cfg.LogAttrs...), attrs...)
}

// errReplyWindowExpired is the cause of a KindFail action. The run error
// carries apperror.CodeChannelReplyWindowExpired.
var errReplyWindowExpired = errors.New("channel reply window expired")
