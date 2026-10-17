package agenttest

import (
	"context"

	"github.com/felinics/memoh/internal/messaging"
)

// MessageSender sends nothing.
type MessageSender struct{}

func (MessageSender) Send(context.Context, string, messaging.Platform, messaging.SendRequest) error {
	return nil
}

// MessageReactor reacts to nothing.
type MessageReactor struct{}

func (MessageReactor) React(context.Context, string, messaging.Platform, messaging.ReactRequest) error {
	return nil
}

// ChannelParser reads every channel name as a platform of that name.
type ChannelParser struct{}

func (ChannelParser) ParseChannelType(raw string) (messaging.Platform, error) {
	return messaging.Platform(raw), nil
}
