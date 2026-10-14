package channel

import "context"

// StreamObserver receives copies of stream events for cross-channel broadcasting
// or external notification (e.g. webhooks). Implementations must be safe for
// concurrent use and should not block.
type StreamObserver interface {
	// OnStreamEvent is called for every event a reply shows.
	// botID identifies the bot that owns the conversation.
	// source is the channel type that originated the event (e.g. "telegram").
	OnStreamEvent(ctx context.Context, botID string, source ChannelType, event StreamEvent)
}
