package delivery

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/channel/adapters/dingtalk"
	"github.com/felinics/memoh/internal/channel/adapters/discord"
	"github.com/felinics/memoh/internal/channel/adapters/feishu"
	"github.com/felinics/memoh/internal/channel/adapters/line"
	localadapter "github.com/felinics/memoh/internal/channel/adapters/local"
	"github.com/felinics/memoh/internal/channel/adapters/matrix"
	"github.com/felinics/memoh/internal/channel/adapters/misskey"
	"github.com/felinics/memoh/internal/channel/adapters/qq"
	"github.com/felinics/memoh/internal/channel/adapters/slack"
	"github.com/felinics/memoh/internal/channel/adapters/telegram"
	"github.com/felinics/memoh/internal/channel/adapters/wechatoa"
	"github.com/felinics/memoh/internal/channel/adapters/wecom"
	"github.com/felinics/memoh/internal/channel/adapters/weixin"
)

// registeredAdapters are the thirteen channel adapters, each with the
// capabilities it declares.
func registeredAdapters() []channel.Adapter {
	return []channel.Adapter{
		dingtalk.NewDingTalkAdapter(nil),
		discord.NewDiscordAdapter(nil),
		feishu.NewFeishuAdapter(nil),
		line.NewAdapter(nil),
		localadapter.NewWebAdapter(nil),
		matrix.NewMatrixAdapter(nil),
		misskey.NewMisskeyAdapter(nil),
		qq.NewQQAdapter(nil),
		slack.NewSlackAdapter(nil),
		telegram.NewTelegramAdapter(nil),
		wechatoa.NewWeChatOAAdapter(nil),
		wecom.NewWeComAdapter(nil),
		weixin.NewWeixinAdapter(nil),
	}
}

// previewShown reports whether a channel shows a reply while it is produced.
func previewShown(c channel.ChannelCapabilities) bool { return c.Preview != channel.PreviewNone }

// The actions Plan returns for each event, by what the adapter declares. The
// expectation is a table over the declared capabilities, not over adapter
// names, so an adapter that changes its declaration is held to the rule for
// the new one.
func TestAdapterCapabilitiesDecideActions(t *testing.T) {
	t.Parallel()

	delta := channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "text", Phase: channel.StreamPhaseText}
	status := channel.StreamEvent{Type: channel.StreamEventStatus, Status: channel.StreamStatusStarted}
	attach := channel.StreamEvent{Type: channel.StreamEventAttachment, Attachments: []channel.Attachment{{Type: "image"}}}
	react := channel.StreamEvent{Type: channel.StreamEventReaction, Reactions: []channel.ReactRequest{{Emoji: "ok"}}}
	failure := channel.StreamEvent{Type: channel.StreamEventError, Error: "boom"}

	events := []struct {
		name  string
		ev    channel.StreamEvent
		start State
		want  func(c channel.ChannelCapabilities) []Kind
	}{
		{"text", delta, state(t0), func(c channel.ChannelCapabilities) []Kind {
			if !previewShown(c) {
				return []Kind{KindObserve}
			}
			return []Kind{KindUpdate}
		}},
		{"status", status, state(t0), func(c channel.ChannelCapabilities) []Kind {
			if !previewShown(c) {
				return []Kind{KindObserve}
			}
			return []Kind{KindUpdate}
		}},
		{"tool status shown", toolStart(), state(t0), func(c channel.ChannelCapabilities) []Kind {
			if !previewShown(c) {
				return []Kind{KindObserve}
			}
			return []Kind{KindUpdate}
		}},
		{"tool status hidden", toolEnd(), hiding(state(t0)), func(channel.ChannelCapabilities) []Kind { return []Kind{KindObserve} }},
		{"attachment", attach, state(t0), func(c channel.ChannelCapabilities) []Kind {
			if c.Preview == channel.PreviewLegacy {
				return []Kind{KindUpdate}
			}
			return []Kind{KindAttach}
		}},
		{"reaction", react, state(t0), func(channel.ChannelCapabilities) []Kind { return []Kind{KindReact} }},
		{"error before text", failure, state(t0), func(c channel.ChannelCapabilities) []Kind {
			if !previewShown(c) {
				return []Kind{KindSend}
			}
			return []Kind{KindUpdate}
		}},
		{"error after text", failure, withStreamed(state(t0)), func(channel.ChannelCapabilities) []Kind { return []Kind{KindSend} }},
		{"approval", approval(), state(t0), func(c channel.ChannelCapabilities) []Kind {
			if c.NativeApproval && previewShown(c) {
				return []Kind{KindUpdate}
			}
			return []Kind{KindSend}
		}},
		{"approval after text", approval(), withStreamed(state(t0)), func(c channel.ChannelCapabilities) []Kind {
			if c.NativeApproval {
				return []Kind{KindUpdate}
			}
			return []Kind{KindEndPreview, KindSend, KindOpenPreview}
		}},
		{"ask_user", askUser(), state(t0), func(c channel.ChannelCapabilities) []Kind {
			if c.NativeUserInput && previewShown(c) {
				return []Kind{KindUpdate}
			}
			return []Kind{KindSend}
		}},
		{"ask_user after text", askUser(), withStreamed(state(t0)), func(c channel.ChannelCapabilities) []Kind {
			if c.NativeUserInput {
				return []Kind{KindUpdate}
			}
			return []Kind{KindEndPreview, KindSend, KindOpenPreview}
		}},
	}
	outputs := []struct {
		name     string
		outputs  []channel.Message
		suppress bool
		start    State
		want     func(c channel.ChannelCapabilities) []Kind
	}{
		{"final", []channel.Message{{Text: "a"}}, false, state(t0), func(c channel.ChannelCapabilities) []Kind {
			if !previewShown(c) {
				return []Kind{KindSend}
			}
			return []Kind{KindFinish}
		}},
		{"several outputs", []channel.Message{{Text: "a"}, {Text: "b"}, {Text: "c"}}, false, state(t0), func(c channel.ChannelCapabilities) []Kind {
			if !previewShown(c) {
				return []Kind{KindSend, KindSend, KindSend}
			}
			return []Kind{KindFinish, KindSend, KindSend}
		}},
		{"suppressed tail", []channel.Message{{Text: "a"}}, true, withStreamed(state(t0)), func(channel.ChannelCapabilities) []Kind {
			return []Kind{KindFinish}
		}},
		{"no output", nil, false, state(t0), func(channel.ChannelCapabilities) []Kind { return []Kind{KindFinish} }},
	}

	for _, adapter := range registeredAdapters() {
		caps := adapter.Descriptor().Capabilities
		t.Run(string(adapter.Type()), func(t *testing.T) {
			t.Parallel()
			for _, tc := range events {
				actions, _ := Plan(tc.ev, streamingCaps(caps), tc.start)
				if want := tc.want(caps); !sameKinds(actions, want...) {
					t.Errorf("%s: actions = %v, want %v", tc.name, kinds(actions), want)
				}
			}
			for _, tc := range outputs {
				actions, _ := PlanEnd(caps, tc.start, tc.outputs, tc.suppress)
				if want := tc.want(caps); !sameKinds(actions, want...) {
					t.Errorf("%s: actions = %v, want %v", tc.name, kinds(actions), want)
				}
			}
		})
	}
}

// streamingCaps is caps as the reply sees them while it streams. The
// adapters' declared reply windows are exercised separately.
func streamingCaps(c channel.ChannelCapabilities) channel.ChannelCapabilities {
	c.ReplyWindow = 0
	return c
}

// An adapter's reply window decides whether a reply is sent as it is, sent
// without its reference, or fails.
func TestAdapterReplyWindowDecidesDelivery(t *testing.T) {
	t.Parallel()

	for _, adapter := range registeredAdapters() {
		caps := adapter.Descriptor().Capabilities
		t.Run(string(adapter.Type()), func(t *testing.T) {
			t.Parallel()
			st := state(t0)
			st.Now = t0.Add(10 * 365 * 24 * time.Hour)
			actions, _ := PlanEnd(caps, st, []channel.Message{{Text: "late", Reply: reply}}, false)
			switch {
			case caps.ReplyWindow <= 0:
				if !sameKinds(actions, KindFinish) || actions[0].Final.Final.Message.Reply != reply {
					t.Fatalf("no window: actions = %+v", actions)
				}
			case caps.ProactiveSend:
				if len(actions) == 0 || actions[0].Kind == KindFail || actions[0].Final.Final.Message.Reply != nil {
					t.Fatalf("proactive send: actions = %+v", actions)
				}
			default:
				if !sameKinds(actions, KindFail) {
					t.Fatalf("expired window: actions = %v, want the run to fail", kinds(actions))
				}
			}
		})
	}
}

// Every adapter gets a request for approval or an answer it can act on: the
// event itself when it declares the native capability, a plain message that
// carries the instructions otherwise. Run through a real manager over the
// adapter's declaration, so the routing is the one production makes.
func TestAdaptersSurfaceApprovalAndUserInputPrompts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		event  channel.StreamEvent
		native func(channel.ChannelCapabilities) bool
		hints  []string
	}{
		{"approval", approval(), func(c channel.ChannelCapabilities) bool { return c.NativeApproval }, []string{"/approve 7", "/reject 7"}},
		{"ask_user", askUser(), func(c channel.ChannelCapabilities) bool { return c.NativeUserInput }, []string{"Pick one", "1) A"}},
	}
	for _, adapter := range registeredAdapters() {
		for _, tc := range cases {
			t.Run(string(adapter.Type())+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				rec := &transport{desc: adapter.Descriptor()}
				rec.desc.Type = adapter.Type()
				sender := managerSender(t, rec)
				r, err := Open(context.Background(), Config{Surface: sender, Target: "target-1"})
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				if err := r.Show(context.Background(), tc.event); err != nil {
					t.Fatalf("plan: %v", err)
				}
				log := rec.snapshot()
				if tc.native(adapter.Descriptor().Capabilities) && previewShown(adapter.Descriptor().Capabilities) {
					if !hasEntry(log, "push:tool_call_start") && !hasEntry(log, "update:tool_call_start") || hasEntry(log, "send") {
						t.Fatalf("native adapter: log = %v, want the event forwarded and no plain message", log)
					}
					return
				}
				if hasEntry(log, "push:") || hasEntry(log, "update:") || len(rec.messages) != 1 {
					t.Fatalf("plain adapter: log = %v messages = %v, want one plain message", log, rec.messages)
				}
				text := rec.messages[0].PlainText()
				for _, want := range tc.hints {
					if !strings.Contains(text, want) {
						t.Fatalf("prompt missing %q:\n%s", want, text)
					}
				}
			})
		}
	}
}
