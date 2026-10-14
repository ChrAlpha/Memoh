package inbound

import (
	"context"
	"testing"

	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/channel/adapters/discord"
	"github.com/felinics/memoh/internal/channel/adapters/feishu"
	"github.com/felinics/memoh/internal/channel/adapters/line"
	"github.com/felinics/memoh/internal/channel/adapters/matrix"
	"github.com/felinics/memoh/internal/channel/adapters/misskey"
	"github.com/felinics/memoh/internal/channel/adapters/slack"
	"github.com/felinics/memoh/internal/channel/adapters/telegram"
	"github.com/felinics/memoh/internal/channel/adapters/weixin"
	"github.com/felinics/memoh/internal/channel/identities"
)

// policyOnlyAdapter is a real adapter's self-identity policy and nothing else,
// so resolving a sender neither looks the sender up on the platform nor
// depends on anything but what the policy declares.
type policyOnlyAdapter struct {
	channel.Adapter
}

func (a policyOnlyAdapter) SelfIdentityPolicy() channel.SelfIdentityPolicy {
	return a.Adapter.(channel.SelfIdentityPolicyProvider).SelfIdentityPolicy()
}

// ownAccountCase is one platform: what the adapter's DiscoverSelf records for
// the bot, and the sender the adapter's inbound code fills in for a message
// from the bot's own account and for one from somebody else.
type ownAccountCase struct {
	name    string
	adapter channel.Adapter
	self    map[string]any
	bot     channel.Identity
	other   channel.Identity
}

func ownAccountCases() []ownAccountCase {
	return []ownAccountCase{
		{
			name:    "telegram",
			adapter: telegram.NewTelegramAdapter(nil),
			self:    map[string]any{"user_id": "777000", "username": "memohbot"},
			bot:     channel.Identity{SubjectID: "777000", Attributes: map[string]string{"chat_id": "-1001", "user_id": "777000", "username": "memohbot"}},
			other:   channel.Identity{SubjectID: "42", Attributes: map[string]string{"chat_id": "-1001", "user_id": "42", "username": "alice"}},
		},
		{
			name:    "feishu",
			adapter: feishu.NewFeishuAdapter(nil),
			self:    map[string]any{"open_id": "ou_bot"},
			bot:     channel.Identity{SubjectID: "ou_bot", Attributes: map[string]string{"open_id": "ou_bot", "user_id": "u_bot"}},
			other:   channel.Identity{SubjectID: "ou_alice", Attributes: map[string]string{"open_id": "ou_alice", "user_id": "u_alice"}},
		},
		{
			name:    "discord",
			adapter: discord.NewDiscordAdapter(nil),
			self:    map[string]any{"user_id": "1100", "username": "memoh"},
			bot:     channel.Identity{SubjectID: "1100", Attributes: map[string]string{"user_id": "1100", "username": "memoh"}},
			other:   channel.Identity{SubjectID: "2200", Attributes: map[string]string{"user_id": "2200", "username": "alice"}},
		},
		{
			name:    "slack",
			adapter: slack.NewSlackAdapter(nil),
			self:    map[string]any{"user_id": "U0BOT", "bot_id": "B0BOT", "team_id": "T1"},
			bot:     channel.Identity{SubjectID: "U0BOT", Attributes: map[string]string{"user_id": "U0BOT"}},
			other:   channel.Identity{SubjectID: "U0ALICE", Attributes: map[string]string{"user_id": "U0ALICE"}},
		},
		{
			name:    "matrix",
			adapter: matrix.NewMatrixAdapter(nil),
			self:    map[string]any{"user_id": "@bot:example.org"},
			bot:     channel.Identity{SubjectID: "@bot:example.org", Attributes: map[string]string{"user_id": "@bot:example.org", "room_id": "!room:example.org"}},
			other:   channel.Identity{SubjectID: "@alice:example.org", Attributes: map[string]string{"user_id": "@alice:example.org", "room_id": "!room:example.org"}},
		},
		{
			name:    "misskey",
			adapter: misskey.NewMisskeyAdapter(nil),
			self:    map[string]any{"user_id": "9abc", "username": "memohbot"},
			bot:     channel.Identity{SubjectID: "9abc", Attributes: map[string]string{"user_id": "9abc", "username": "memohbot"}},
			other:   channel.Identity{SubjectID: "9xyz", Attributes: map[string]string{"user_id": "9xyz", "username": "alice"}},
		},
		{
			// LINE records the bot under bot_user_id and reports senders as
			// user_id; both are the same LINE user id.
			name:    "line",
			adapter: line.NewAdapter(nil),
			self:    map[string]any{"bot_user_id": "U0123456789abcdef0123456789abcdef", "basic_id": "@memoh"},
			bot:     channel.Identity{SubjectID: "U0123456789abcdef0123456789abcdef", Attributes: map[string]string{"user_id": "U0123456789abcdef0123456789abcdef"}},
			other:   channel.Identity{SubjectID: "Ufedcba9876543210fedcba9876543210", Attributes: map[string]string{"user_id": "Ufedcba9876543210fedcba9876543210"}},
		},
	}
}

func newOwnAccountResolver(t *testing.T, tc ownAccountCase) (*IdentityResolver, *fakeChannelIdentityService) {
	t.Helper()
	registry := channel.NewRegistry()
	registry.MustRegister(policyOnlyAdapter{Adapter: tc.adapter})
	identitySvc := &fakeChannelIdentityService{channelIdentity: identities.ChannelIdentity{ID: "ci-1"}}
	return NewIdentityResolver(nil, registry, identitySvc, &fakePolicyService{}, ""), identitySvc
}

func ownAccountMessage(tc ownAccountCase, sender channel.Identity) channel.InboundMessage {
	return channel.InboundMessage{
		BotID:        "bot-1",
		Channel:      tc.adapter.Type(),
		Message:      channel.Message{ID: "m1", Text: "hello"},
		ReplyTarget:  "target-1",
		Sender:       sender,
		Conversation: channel.Conversation{ID: "conv-1", Type: channel.ConversationTypeGroup},
	}
}

// A message from the account the bot runs as is dropped before the sender
// is given an identity, on every platform that records an id for it.
func TestResolveStopsMessagesFromTheBotsOwnAccount(t *testing.T) {
	t.Parallel()
	for _, tc := range ownAccountCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resolver, identitySvc := newOwnAccountResolver(t, tc)
			cfg := channel.ChannelConfig{ID: "cfg-1", BotID: "bot-1", ChannelType: tc.adapter.Type(), SelfIdentity: tc.self}

			state, err := resolver.Resolve(context.Background(), cfg, ownAccountMessage(tc, tc.bot))
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if state.Decision == nil || !state.Decision.Stop || !state.Decision.Reply.IsEmpty() {
				t.Fatalf("Decision = %+v, want a silent stop", state.Decision)
			}
			if identitySvc.calls != 0 || state.Identity.ChannelIdentityID != "" {
				t.Fatalf("the bot's own account was given an identity (%d lookups, id %q)", identitySvc.calls, state.Identity.ChannelIdentityID)
			}

			state, err = resolver.Resolve(context.Background(), cfg, ownAccountMessage(tc, tc.other))
			if err != nil {
				t.Fatalf("Resolve(other sender) error = %v", err)
			}
			if state.Decision != nil || state.Identity.ChannelIdentityID != "ci-1" {
				t.Fatalf("another sender: Decision = %+v, identity %q, want resolved and not stopped", state.Decision, state.Identity.ChannelIdentityID)
			}
		})
	}
}

// A config that has not recorded the bot's id has nothing to compare with, so
// no sender is taken for the bot.
func TestResolveWithoutRecordedSelfIdentityStopsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range ownAccountCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resolver, _ := newOwnAccountResolver(t, tc)
			cfg := channel.ChannelConfig{ID: "cfg-1", BotID: "bot-1", ChannelType: tc.adapter.Type(), SelfIdentity: map[string]any{}}

			state, err := resolver.Resolve(context.Background(), cfg, ownAccountMessage(tc, tc.bot))
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if state.Decision != nil {
				t.Fatalf("Decision = %+v, want none without a recorded id", state.Decision)
			}
		})
	}
}

// Weixin is owner-only and records no id for the bot, so the platform's own
// sender type is all there is to tell the bot's message apart. One it marks as
// the bot's is dropped before any identity or owner account is resolved for it;
// one from the user is resolved as usual, as the owner.
func TestResolveStopsMessagesThePlatformMarksAsTheBots(t *testing.T) {
	t.Parallel()
	registry := channel.NewRegistry()
	registry.MustRegister(weixin.NewWeixinAdapter(nil))
	identitySvc := &fakeChannelIdentityService{channelIdentity: identities.ChannelIdentity{ID: "ci-1"}}
	resolver := NewIdentityResolver(nil, registry, identitySvc, &fakePolicyService{ownerUserID: "owner-1"}, "")
	cfg := channel.ChannelConfig{ID: "cfg-1", BotID: "bot-1", ChannelType: weixin.Type}
	message := func(sentByBot bool) channel.InboundMessage {
		return channel.InboundMessage{
			BotID:        "bot-1",
			Channel:      weixin.Type,
			Message:      channel.Message{ID: "m1", Text: "hello"},
			ReplyTarget:  "user1@im.wechat",
			Sender:       channel.Identity{SubjectID: "user1@im.wechat", Attributes: map[string]string{"user_id": "user1@im.wechat"}},
			Conversation: channel.Conversation{ID: "user1@im.wechat", Type: channel.ConversationTypePrivate},
			SentByBot:    sentByBot,
		}
	}

	state, err := resolver.Resolve(context.Background(), cfg, message(true))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if state.Decision == nil || !state.Decision.Stop || !state.Decision.Reply.IsEmpty() {
		t.Fatalf("Decision = %+v, want a silent stop", state.Decision)
	}
	if identitySvc.calls != 0 || state.Identity.ChannelIdentityID != "" || state.Identity.UserID != "" {
		t.Fatalf("the bot's message was given an identity (%d lookups, id %q, user %q)",
			identitySvc.calls, state.Identity.ChannelIdentityID, state.Identity.UserID)
	}

	state, err = resolver.Resolve(context.Background(), cfg, message(false))
	if err != nil {
		t.Fatalf("Resolve(user message) error = %v", err)
	}
	if state.Decision != nil || state.Identity.ChannelIdentityID != "ci-1" || state.Identity.UserID != "owner-1" {
		t.Fatalf("user message: Decision = %+v, identity %q, user %q, want resolved as the owner and not stopped",
			state.Decision, state.Identity.ChannelIdentityID, state.Identity.UserID)
	}
}
