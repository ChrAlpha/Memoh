package channel

import (
	"context"
	"errors"
)

// Preview is one reply being shown while it is produced. The delivery layer
// opens it, forwards the reply's events with Update, and ends it exactly once
// with Finish or Abort.
type Preview interface {
	// Update forwards an event to the open reply.
	Update(ctx context.Context, event StreamEvent) error
	// Finish ends the reply. final is the reply's last event, or nil when the
	// reply has no message of its own.
	Finish(ctx context.Context, final *StreamEvent) error
	// Abort ends the reply because the run failed. Text the reply already
	// shows stays; nothing more is added to it.
	Abort(ctx context.Context) error
}

// PreparedPreview is the adapter side of Preview. Events reach it prepared:
// attachments are resolved, text is within the platform's size, and the final
// is in a format the channel renders. The manager owns that work for every
// adapter, so a preview only shows what it is given.
//
// Update receives the reply's status, text deltas, phases and tool events. An
// error event is the reply's terminal state when no text has been shown: the
// preview displays it in place of the placeholder. Finish is called once with
// the final message. A final with an empty message ends the reply as it
// stands; a nil final means the run said nothing, and the preview removes
// whatever placeholder it shows. Abort is called when the reply ends before a
// final, and the preview shows nothing more.
type PreparedPreview interface {
	Update(ctx context.Context, event PreparedStreamEvent) error
	Finish(ctx context.Context, final *PreparedStreamEvent) error
	Abort(ctx context.Context) error
}

// PreviewOpener is implemented by an adapter whose Capabilities().Preview is
// not PreviewLegacy.
type PreviewOpener interface {
	OpenPreview(ctx context.Context, cfg ChannelConfig, target string, opts StreamOptions) (PreparedPreview, error)
}

// openAdapterPreview opens the preview of an adapter that implements
// PreviewOpener, on a resolved target.
func (s *managerSurface) openAdapterPreview(ctx context.Context, target string, opts StreamOptions) (*managerOutboundStream, error) {
	adapter, _ := s.manager.registry.Get(s.channelType)
	opener, ok := adapter.(PreviewOpener)
	if !ok {
		return nil, errors.New("channel adapter does not open previews")
	}
	raw, err := opener.OpenPreview(ctx, s.config, target, opts)
	if err != nil {
		return nil, err
	}
	return s.newOutboundStream(target, opts, &previewSession{preview: raw}, func(ctx context.Context) (PreparedOutboundStream, error) {
		raw, err := opener.OpenPreview(ctx, s.config, target, continuation(opts))
		if err != nil {
			return nil, err
		}
		return &previewSession{preview: raw}, nil
	}), nil
}

// previewSession drives a PreparedPreview through the Push/Close session the
// manager's chunking and splitting are written against. A final ends the
// preview with it, a completed status without a final ends it with none, and
// closing a session that did neither aborts it.
type previewSession struct {
	preview PreparedPreview
	ended   bool
}

func (p *previewSession) Push(ctx context.Context, event PreparedStreamEvent) error {
	if p.ended {
		return nil
	}
	switch {
	case event.Type == StreamEventFinal && event.Final != nil:
		p.ended = true
		return p.preview.Finish(ctx, &event)
	case event.Type == StreamEventStatus && event.Status == StreamStatusCompleted:
		p.ended = true
		return p.preview.Finish(ctx, nil)
	default:
		return p.preview.Update(ctx, event)
	}
}

func (p *previewSession) Close(ctx context.Context) error {
	if p.ended {
		return nil
	}
	p.ended = true
	return p.preview.Abort(ctx)
}

// Update forwards event to the adapter.
func (s *managerOutboundStream) Update(ctx context.Context, event StreamEvent) error {
	return s.Push(ctx, event)
}

// Finish pushes final when there is one, then the completed status, and closes
// the session whether or not the pushes succeeded. The close does not follow a
// canceled ctx, so a session is never left open.
func (s *managerOutboundStream) Finish(ctx context.Context, final *StreamEvent) error {
	err := s.pushEnd(ctx, final)
	return errors.Join(err, s.Close(context.WithoutCancel(ctx)))
}

func (s *managerOutboundStream) pushEnd(ctx context.Context, final *StreamEvent) error {
	if final != nil {
		if err := s.Push(ctx, *final); err != nil {
			return err
		}
	}
	return s.Push(ctx, StreamEvent{Type: StreamEventStatus, Status: StreamStatusCompleted})
}

// Abort flushes the text the adapter buffered, so an error sent after it does
// not lose that text, then closes the session. A session that carried no text
// has nothing to flush.
func (s *managerOutboundStream) Abort(ctx context.Context) error {
	var err error
	if s.deltaRunes > 0 {
		err = s.Push(ctx, StreamEvent{Type: StreamEventFinal, Final: &StreamFinalizePayload{}})
	}
	return errors.Join(err, s.Close(context.WithoutCancel(ctx)))
}
