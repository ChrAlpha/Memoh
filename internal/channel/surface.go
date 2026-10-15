package channel

import (
	"context"
	"errors"
	"strings"
)

// managerSurface is what the manager offers for the replies to one inbound
// message: whole messages, attachments, reactions and streamed previews, all
// for one config. It satisfies delivery.Surface and StreamReplySender, and is
// the only place that turns a reply into adapter calls.
type managerSurface struct {
	manager     *Manager
	sender      Sender
	channelType ChannelType
	config      ChannelConfig
}

func (m *Manager) newSurface(cfg ChannelConfig, channelType ChannelType) *managerSurface {
	sender, _ := m.registry.GetSender(channelType)
	return &managerSurface{
		manager:     m,
		sender:      sender,
		channelType: channelType,
		config:      cfg,
	}
}

// Capabilities returns the channel's capabilities as refined for target.
func (s *managerSurface) Capabilities(target string) ChannelCapabilities {
	caps, _ := s.manager.registry.GetOutboundCapabilities(s.channelType, s.config, target)
	return caps
}

// Send delivers msg to its target after resolving the target's alias.
func (s *managerSurface) Send(ctx context.Context, msg OutboundMessage) error {
	if s.manager == nil {
		return errors.New("channel manager not configured")
	}
	target, err := s.manager.resolveOutboundTarget(ctx, s.channelType, s.config, msg.Target)
	if err != nil {
		return err
	}
	msg.Target = target
	return s.deliver(ctx, msg)
}

// deliver sends msg to its resolved target. It is the one path that splits a
// message into the platform's size, downgrades its format and buttons to what
// the channel renders, prepares its attachments and retries the send.
// Manager.Send, replies of an inbound message and the overflow of a streamed
// final all end here.
func (s *managerSurface) deliver(ctx context.Context, msg OutboundMessage) error {
	policy := s.manager.resolveOutboundPolicy(s.channelType)
	caps, hasCaps := s.manager.registry.GetOutboundCapabilities(s.channelType, s.config, msg.Target)
	outbound, err := buildOutboundMessagesWithCaps(msg, policy, caps, hasCaps)
	if err != nil {
		return err
	}
	return s.manager.sendAllWithConfig(ctx, s.sender, s.config, outbound, policy)
}

// Attach sends att to target as a message of its own, quoting reply when it is
// not nil.
func (s *managerSurface) Attach(ctx context.Context, target string, att Attachment, reply *ReplyRef) error {
	return s.Send(ctx, OutboundMessage{Target: target, Message: Message{Attachments: []Attachment{att}, Reply: reply}})
}

// React adds or removes a reaction.
func (s *managerSurface) React(ctx context.Context, req ReactRequest) error {
	reactor, ok := s.manager.registry.GetReactor(s.channelType)
	if !ok {
		return errors.New("channel " + s.channelType.String() + " does not support reactions")
	}
	return s.manager.react(ctx, reactor, s.config, s.channelType, req)
}

// OpenPreview opens the reply's preview on target through the adapter's
// PreviewOpener, wrapped in the manager's chunking and stream splitting.
func (s *managerSurface) OpenPreview(ctx context.Context, target string, opts StreamOptions) (Preview, error) {
	target, err := s.previewTarget(ctx, target)
	if err != nil {
		return nil, err
	}
	adapter, _ := s.manager.registry.Get(s.channelType)
	opener, ok := adapter.(PreviewOpener)
	if !ok {
		return nil, errors.New("channel adapter does not open previews")
	}
	raw, err := opener.OpenPreview(ctx, s.config, target, opts)
	if err != nil {
		return nil, err
	}
	return s.newOutboundStream(target, opts, raw, func(ctx context.Context) (PreparedPreview, error) {
		return opener.OpenPreview(ctx, s.config, target, continuation(opts))
	}), nil
}

// previewTarget resolves target's alias: a channel that cannot stream has no
// preview.
func (s *managerSurface) previewTarget(ctx context.Context, target string) (string, error) {
	if s.manager == nil {
		return "", errors.New("channel manager not configured")
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return "", errors.New("target is required")
	}
	target, err := s.manager.resolveOutboundTarget(ctx, s.channelType, s.config, target)
	if err != nil {
		return "", err
	}
	caps := s.Capabilities(target)
	if !caps.Streaming && !caps.BlockStreaming {
		return "", errors.New("channel does not support streaming")
	}
	return target, nil
}

// newOutboundStream wraps an adapter preview in the manager's chunking and
// stream splitting. A continuation stream carries the rest of a reply whose
// earlier text is already delivered, so its final does not repeat that text.
func (s *managerSurface) newOutboundStream(target string, opts StreamOptions, preview PreparedPreview, reopen func(context.Context) (PreparedPreview, error)) *managerOutboundStream {
	stream := &managerOutboundStream{
		manager:     s.manager,
		config:      s.config,
		preview:     preview,
		channelType: s.channelType,
		target:      target,
		policy:      s.manager.resolveOutboundPolicy(s.channelType),
		sender:      s.sender,
		send: func(ctx context.Context, msg OutboundMessage) error {
			msg.Target = target
			return s.deliver(ctx, msg)
		},
		reopen: reopen,
	}
	if opts.Continuation {
		stream.splitCount = 1
	}
	return stream
}

// continuation is opts for the stream that carries on after a split: it does
// not quote the source message again.
func continuation(opts StreamOptions) StreamOptions {
	return StreamOptions{
		SourceMessageID: opts.SourceMessageID,
		Metadata:        opts.Metadata,
		Continuation:    true,
	}
}
