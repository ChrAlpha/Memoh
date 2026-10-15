package delivery

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/apperror"
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

// capabilitySet is a declaration the contract is held against.
type capabilitySet struct {
	name string
	caps channel.ChannelCapabilities
}

var previewModes = []struct {
	name string
	mode channel.PreviewMode
}{
	{"none", channel.PreviewNone},
	{"edit_in_place", channel.PreviewEditInPlace},
	{"native_stream", channel.PreviewNativeStream},
	{"draft", channel.PreviewDraft},
}

// declaredCapabilities are the capabilities each registered adapter declares.
func declaredCapabilities() []capabilitySet {
	var sets []capabilitySet
	for _, adapter := range registeredAdapters() {
		sets = append(sets, capabilitySet{string(adapter.Type()), adapter.Descriptor().Capabilities})
	}
	return sets
}

// modeCapabilities has one declaration for each preview mode, with and without
// native prompts, so the rule for a mode no adapter declares yet is held too.
func modeCapabilities() []capabilitySet {
	var sets []capabilitySet
	for _, m := range previewModes {
		sets = append(sets,
			capabilitySet{"mode/" + m.name, channel.ChannelCapabilities{Preview: m.mode}},
			capabilitySet{"mode/" + m.name + "+native", channel.ChannelCapabilities{Preview: m.mode, NativeApproval: true, NativeUserInput: true}})
	}
	return sets
}

// windowCapabilities has one declaration for each preview mode with a reply
// window, with and without proactive send.
func windowCapabilities() []capabilitySet {
	var sets []capabilitySet
	for _, m := range previewModes {
		sets = append(sets,
			capabilitySet{"window/" + m.name, channel.ChannelCapabilities{Preview: m.mode, ReplyWindow: time.Hour}},
			capabilitySet{"window/" + m.name + "+proactive", channel.ChannelCapabilities{Preview: m.mode, ReplyWindow: time.Hour, ProactiveSend: true}})
	}
	return sets
}

func joined(lists ...[]capabilitySet) []capabilitySet {
	var all []capabilitySet
	for _, l := range lists {
		all = append(all, l...)
	}
	return all
}

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

	// The state once the reply has text: the preview shows it, or a channel
	// without a preview holds it.
	fresh := func(channel.ChannelCapabilities) State { return state(t0) }
	hidden := func(channel.ChannelCapabilities) State { return hiding(state(t0)) }
	afterText := func(c channel.ChannelCapabilities) State {
		if previewShown(c) {
			return withStreamed(state(t0))
		}
		return withPending(state(t0), "text")
	}
	shown := func(c channel.ChannelCapabilities) []Kind {
		if !previewShown(c) {
			return []Kind{KindObserve}
		}
		return []Kind{KindUpdate}
	}
	prompt := func(native func(channel.ChannelCapabilities) bool, split bool) func(channel.ChannelCapabilities) []Kind {
		return func(c channel.ChannelCapabilities) []Kind {
			switch {
			case !previewShown(c):
				return []Kind{KindSend}
			case native(c):
				return []Kind{KindUpdate}
			case split:
				return []Kind{KindEndPreview, KindSend, KindOpenPreview}
			}
			return []Kind{KindSend}
		}
	}
	nativeApproval := func(c channel.ChannelCapabilities) bool { return c.NativeApproval }
	nativeInput := func(c channel.ChannelCapabilities) bool { return c.NativeUserInput }

	events := []struct {
		name  string
		ev    channel.StreamEvent
		start func(channel.ChannelCapabilities) State
		want  func(c channel.ChannelCapabilities) []Kind
	}{
		{"text", delta, fresh, shown},
		{"status", status, fresh, shown},
		{"tool start shown", toolStart(), fresh, shown},
		{"tool result shown", toolEnd(), fresh, func(c channel.ChannelCapabilities) []Kind {
			if !previewShown(c) {
				return []Kind{KindSend}
			}
			return []Kind{KindUpdate}
		}},
		{"tool start hidden", toolStart(), hidden, func(channel.ChannelCapabilities) []Kind { return []Kind{KindObserve} }},
		{"tool result hidden", toolEnd(), hidden, func(channel.ChannelCapabilities) []Kind { return []Kind{KindObserve} }},
		{"attachment", attach, fresh, func(channel.ChannelCapabilities) []Kind { return []Kind{KindAttach} }},
		{"attachment after text", attach, afterText, func(c channel.ChannelCapabilities) []Kind {
			if c.Preview == channel.PreviewNone {
				return []Kind{KindSend, KindAttach}
			}
			return []Kind{KindAttach}
		}},
		{"reaction", react, fresh, func(channel.ChannelCapabilities) []Kind { return []Kind{KindReact} }},
		{"error before text", failure, fresh, func(c channel.ChannelCapabilities) []Kind {
			if !previewShown(c) {
				return []Kind{KindSend}
			}
			return []Kind{KindUpdate}
		}},
		{"error after text", failure, afterText, func(channel.ChannelCapabilities) []Kind { return []Kind{KindSend} }},
		{"approval", approval(), fresh, prompt(nativeApproval, false)},
		{"approval after text", approval(), afterText, prompt(nativeApproval, true)},
		{"ask_user", askUser(), fresh, prompt(nativeInput, false)},
		{"ask_user after text", askUser(), afterText, prompt(nativeInput, true)},
	}
	outputs := []struct {
		name     string
		outputs  []channel.Message
		suppress bool
		start    func(channel.ChannelCapabilities) State
		want     func(c channel.ChannelCapabilities) []Kind
	}{
		{"final", []channel.Message{{Text: "a"}}, false, fresh, func(c channel.ChannelCapabilities) []Kind {
			if !previewShown(c) {
				return []Kind{KindSend}
			}
			return []Kind{KindFinish}
		}},
		{"several outputs", []channel.Message{{Text: "a"}, {Text: "b"}, {Text: "c"}}, false, fresh, func(c channel.ChannelCapabilities) []Kind {
			if !previewShown(c) {
				return []Kind{KindSend, KindSend, KindSend}
			}
			return []Kind{KindFinish, KindSend, KindSend}
		}},
		{"suppressed tail", []channel.Message{{Text: "a"}}, true, afterText, func(channel.ChannelCapabilities) []Kind {
			return []Kind{KindFinish}
		}},
		{"no output", nil, false, fresh, func(channel.ChannelCapabilities) []Kind { return []Kind{KindFinish} }},
	}

	for _, set := range joined(declaredCapabilities(), modeCapabilities()) {
		t.Run(set.name, func(t *testing.T) {
			t.Parallel()
			caps := set.caps
			for _, tc := range events {
				actions, _ := Plan(tc.ev, streamingCaps(caps), tc.start(caps))
				if want := tc.want(caps); !sameKinds(actions, want...) {
					t.Errorf("%s: actions = %v, want %v", tc.name, kinds(actions), want)
				}
			}
			for _, tc := range outputs {
				actions, _ := PlanEnd(caps, tc.start(caps), tc.outputs, tc.suppress)
				if want := tc.want(caps); !sameKinds(actions, want...) {
					t.Errorf("%s: actions = %v, want %v", tc.name, kinds(actions), want)
				}
			}
		})
	}
}

// sentReplyOf is the reply reference of the message an action sends: the final
// of a preview, a standalone message, or the reference an attachment quotes.
func sentReplyOf(a Action) *channel.ReplyRef {
	switch a.Kind {
	case KindFinish:
		return a.Final.Final.Message.Reply
	case KindAttach:
		return a.Reply
	}
	return a.Message.Message.Reply
}

// streamingCaps is caps as the reply sees them while it streams. The
// adapters' declared reply windows are exercised separately.
func streamingCaps(c channel.ChannelCapabilities) channel.ChannelCapabilities {
	c.ReplyWindow = 0
	return c
}

// An adapter's reply window decides whether a reply is sent as it is, sent
// without its reference, or fails. The rule holds for every message of the
// reply: its outputs, its attachments, and the state of its tool calls.
func TestAdapterReplyWindowDecidesDelivery(t *testing.T) {
	t.Parallel()

	attach := channel.StreamEvent{Type: channel.StreamEventAttachment, Attachments: []channel.Attachment{{Type: "image"}}}
	for _, set := range joined(declaredCapabilities(), windowCapabilities()) {
		caps := set.caps
		t.Run(set.name, func(t *testing.T) {
			t.Parallel()
			st := state(t0)
			st.Now = t0.Add(10 * 365 * 24 * time.Hour)
			check := func(what string, actions []Action) {
				t.Helper()
				switch {
				case caps.ReplyWindow <= 0:
					if len(actions) != 1 || sentReplyOf(actions[0]) != reply {
						t.Fatalf("%s, no window: actions = %+v", what, actions)
					}
				case caps.ProactiveSend:
					if len(actions) != 1 || actions[0].Kind == KindFail || sentReplyOf(actions[0]) != nil {
						t.Fatalf("%s, proactive send: actions = %+v", what, actions)
					}
				default:
					if !sameKinds(actions, KindFail) {
						t.Fatalf("%s, expired window: actions = %v, want the run to fail", what, kinds(actions))
					}
				}
			}
			ended, _ := PlanEnd(caps, st, []channel.Message{{Text: "late", Reply: reply}}, false)
			check("output", ended)
			attached, _ := Plan(attach, caps, st)
			check("attachment", attached)
			if caps.Preview == channel.PreviewNone {
				result, _ := Plan(toolEnd(), caps, st)
				check("tool result", result)
			}
		})
	}
}

// The preview opens with the reference the window allows: the source message
// inside it, none after it when the platform accepts a proactive message, and
// no preview when it does not. A channel without a preview opens none.
func TestAdapterReplyWindowDecidesThePreviewReference(t *testing.T) {
	t.Parallel()

	for _, set := range joined(declaredCapabilities(), windowCapabilities()) {
		caps := set.caps
		t.Run(set.name, func(t *testing.T) {
			t.Parallel()
			surface := &surfaceLog{caps: caps}
			_, err := Open(context.Background(), Config{
				Surface: surface, Target: "chat-1", Reply: reply, ReceivedAt: t0,
				Now: func() time.Time { return t0.Add(10 * 365 * 24 * time.Hour) },
			})
			switch {
			case !previewShown(caps):
				if err != nil || len(surface.opens) != 0 {
					t.Fatalf("no preview: opens = %+v, err = %v", surface.opens, err)
				}
			case caps.ReplyWindow <= 0:
				if err != nil || len(surface.opens) != 1 || surface.opens[0].Reply != reply {
					t.Fatalf("no window: opens = %+v, err = %v", surface.opens, err)
				}
			case caps.ProactiveSend:
				if err != nil || len(surface.opens) != 1 || surface.opens[0].Reply != nil {
					t.Fatalf("proactive send: opens = %+v, err = %v", surface.opens, err)
				}
			default:
				if apperror.CodeOf(err) != apperror.CodeChannelReplyWindowExpired || len(surface.opens) != 0 {
					t.Fatalf("expired window: opens = %+v, err = %v", surface.opens, err)
				}
			}
		})
	}
}

// An adapter shows a reply through a preview exactly when it declares a preview
// mode and opens previews. The zero mode is PreviewNone, so an adapter that
// leaves the declaration out reads as one that shows none, and the check fails
// when it implements PreviewOpener all the same.
func TestAdaptersDeclareThePreviewTheyImplement(t *testing.T) {
	t.Parallel()

	for _, adapter := range registeredAdapters() {
		t.Run(string(adapter.Type()), func(t *testing.T) {
			t.Parallel()
			_, opens := adapter.(channel.PreviewOpener)
			declared := adapter.Descriptor().Capabilities.Preview != channel.PreviewNone
			switch {
			case opens && !declared:
				t.Error("implements PreviewOpener but declares no preview mode")
			case declared && !opens:
				t.Error("declares a preview mode but does not implement PreviewOpener")
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
				r, err := Open(context.Background(), Config{Surface: sender, Target: "target-1", ReceivedAt: time.Now()})
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				if err := r.Show(context.Background(), tc.event); err != nil {
					t.Fatalf("plan: %v", err)
				}
				log := rec.snapshot()
				if tc.native(adapter.Descriptor().Capabilities) && previewShown(adapter.Descriptor().Capabilities) {
					if !hasEntry(log, "update:tool_call_start") || hasEntry(log, "send") {
						t.Fatalf("native adapter: log = %v, want the event forwarded and no plain message", log)
					}
					return
				}
				if hasEntry(log, "update:") || len(rec.messages) != 1 {
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
