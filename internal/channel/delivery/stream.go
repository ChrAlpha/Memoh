package delivery

import (
	"context"

	"github.com/felinics/memoh/internal/channel"
)

// OpenStream opens a reply for a caller that produces the events itself. It
// pushes the events the reply shows, then each message of the reply as a final
// event, then the completed status; Close ends a reply that did not complete.
// The reply follows the rules of a run's reply: the first message ends the
// preview and each later one is a message of its own.
func OpenStream(ctx context.Context, cfg Config) (channel.OutboundStream, error) {
	reply, err := Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &replyStream{reply: reply}, nil
}

type replyStream struct {
	reply   *Reply
	outputs []channel.Message
}

func (s *replyStream) Push(ctx context.Context, event channel.StreamEvent) error {
	switch {
	case event.Type == channel.StreamEventFinal:
		if event.Final != nil && !event.Final.Message.IsEmpty() {
			s.outputs = append(s.outputs, event.Final.Message)
		}
		return nil
	case event.Type == channel.StreamEventStatus && event.Status == channel.StreamStatusCompleted:
		return s.reply.End(ctx, s.outputs, false)
	}
	return s.reply.Show(ctx, event)
}

func (s *replyStream) Close(ctx context.Context) error {
	return s.reply.Close(ctx)
}
