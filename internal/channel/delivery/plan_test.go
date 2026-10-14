package delivery

import (
	"strings"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/channel"
)

var (
	t0      = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	reply   = &channel.ReplyRef{Target: "chat-1", MessageID: "m-1"}
	noCaps  = channel.ChannelCapabilities{}
	winCaps = channel.ChannelCapabilities{ReplyWindow: time.Hour}
)

func kinds(actions []Action) []Kind {
	out := make([]Kind, 0, len(actions))
	for _, a := range actions {
		out = append(out, a.Kind)
	}
	return out
}

func sameKinds(got []Action, want ...Kind) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i].Kind != want[i] {
			return false
		}
	}
	return true
}

func state(now time.Time) State {
	return State{Target: "chat-1", Reply: reply, SourceMessageID: "m-1", ReceivedAt: t0, Now: now}
}

func withStreamed(st State) State {
	st.streamed = true
	return st
}

func holding(st State) State {
	st.HoldErrors = true
	return st
}

func hiding(st State) State {
	st.HideToolCalls = true
	return st
}

func approval() channel.StreamEvent {
	return channel.StreamEvent{Type: channel.StreamEventToolCallStart, ToolCall: &channel.StreamToolCall{
		Name: "exec", CallID: "call-1", ApprovalID: "approval-1", ShortID: 7,
		Input: map[string]any{"command": "pwd"},
	}}
}

func askUser() channel.StreamEvent {
	return channel.StreamEvent{Type: channel.StreamEventToolCallStart, ToolCall: &channel.StreamToolCall{
		Name: "ask_user", CallID: "call-2",
		Actions: []channel.Action{{Type: "user_input", Value: "respond:input-1"}},
		Input: map[string]any{"payload": map[string]any{"questions": []any{map[string]any{
			"text": "Pick one", "kind": "single_select",
			"options": []any{map[string]any{"label": "A"}, map[string]any{"label": "B"}},
		}}}},
	}}
}

func toolStart() channel.StreamEvent {
	return channel.StreamEvent{Type: channel.StreamEventToolCallStart, ToolCall: &channel.StreamToolCall{Name: "search", CallID: "call-3"}}
}

func toolEnd() channel.StreamEvent {
	return channel.StreamEvent{Type: channel.StreamEventToolCallEnd, ToolCall: &channel.StreamToolCall{Name: "search", CallID: "call-3"}}
}

// Contract: an event's action depends only on the event, the capabilities and
// the reply state.
func TestPlanEventActions(t *testing.T) {
	delta := channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "partial", Phase: channel.StreamPhaseText}
	thinking := channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "hmm", Phase: channel.StreamPhaseReasoning}
	errEv := channel.StreamEvent{Type: channel.StreamEventError, Error: "boom"}
	empty := channel.StreamEvent{Type: channel.StreamEventError, Error: " "}
	status := channel.StreamEvent{Type: channel.StreamEventStatus, Status: channel.StreamStatusStarted}
	react := channel.StreamEvent{Type: channel.StreamEventReaction, Reactions: []channel.ReactRequest{{Emoji: "ok"}}}
	noReact := channel.StreamEvent{Type: channel.StreamEventReaction}
	attach := channel.StreamEvent{Type: channel.StreamEventAttachment, Attachments: []channel.Attachment{{Type: "image"}}}
	reset := channel.StreamEvent{Type: channel.StreamEventReset}

	cases := []struct {
		name  string
		ev    channel.StreamEvent
		start State
		want  []Kind
	}{
		{"status is forwarded to the preview", status, state(t0), []Kind{KindUpdate}},
		{"attachment is forwarded to a legacy preview", attach, state(t0), []Kind{KindUpdate}},
		{"delta is forwarded to the preview", delta, state(t0), []Kind{KindUpdate}},
		{"error before any text goes through the preview", errEv, state(t0), []Kind{KindUpdate}},
		{"error after streamed text is a new message", errEv, withStreamed(state(t0)), []Kind{KindSend}},
		{"blank error is dropped", empty, state(t0), nil},
		{"reaction is dispatched", react, state(t0), []Kind{KindReact}},
		{"reaction without items is dropped", noReact, state(t0), nil},
		{"held error waits for the run end", errEv, holding(state(t0)), nil},
		{"ordinary tool events reach the preview", toolStart(), state(t0), []Kind{KindUpdate}},
		{"hidden tool start is shown to observers only", toolStart(), hiding(state(t0)), []Kind{KindObserve}},
		{"hidden tool end is shown to observers only", toolEnd(), hiding(state(t0)), []Kind{KindObserve}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actions, _ := Plan(tc.ev, noCaps, tc.start)
			if !sameKinds(actions, tc.want...) {
				t.Fatalf("actions = %v, want %v", kinds(actions), tc.want)
			}
		})
	}

	t.Run("reasoning text is not preview text", func(t *testing.T) {
		_, st := Plan(thinking, noCaps, state(t0))
		actions, _ := Plan(errEv, noCaps, st)
		if !sameKinds(actions, KindUpdate) {
			t.Fatalf("error after reasoning = %v, want the preview to end with it", kinds(actions))
		}
	})
	t.Run("visible text makes a later error a message", func(t *testing.T) {
		_, st := Plan(delta, noCaps, state(t0))
		actions, _ := Plan(errEv, noCaps, st)
		if !sameKinds(actions, KindSend) {
			t.Fatalf("error after text = %v, want a message", kinds(actions))
		}
	})
	t.Run("a reset discards the text the preview shows", func(t *testing.T) {
		_, st := Plan(delta, noCaps, state(t0))
		_, st = Plan(reset, noCaps, st)
		actions, _ := Plan(errEv, noCaps, st)
		if !sameKinds(actions, KindUpdate) {
			t.Fatalf("error after reset = %v, want the preview to end with it", kinds(actions))
		}
	})
	t.Run("an error is a message that quotes the source message", func(t *testing.T) {
		actions, _ := Plan(errEv, noCaps, withStreamed(state(t0)))
		got := actions[0].Message
		if got.Target != "chat-1" || got.Message.Text != "Error: boom" || got.Message.Reply != reply {
			t.Fatalf("error message = %+v", got)
		}
		if actions[0].Event.Type != channel.StreamEventError {
			t.Fatalf("observers see %+v, want the error event", actions[0].Event)
		}
	})
	t.Run("reactions are addressed to the source message", func(t *testing.T) {
		actions, _ := Plan(react, noCaps, state(t0))
		got := actions[0].Reactions
		if len(got) != 1 || got[0].Target != "chat-1" || got[0].MessageID != "m-1" || got[0].Emoji != "ok" {
			t.Fatalf("reactions = %+v", got)
		}
	})
}

// Contract for requests the user answers: a channel that renders them hands
// them to the preview; any other channel gets a plain message. When the
// preview shows text, it ends first and a new preview carries what follows.
func TestPlanPrompts(t *testing.T) {
	native := channel.ChannelCapabilities{NativeApproval: true, NativeUserInput: true}
	approvalOnly := channel.ChannelCapabilities{NativeApproval: true}
	inputOnly := channel.ChannelCapabilities{NativeUserInput: true}

	cases := []struct {
		name  string
		ev    channel.StreamEvent
		caps  channel.ChannelCapabilities
		start State
		want  []Kind
	}{
		{"native approval goes to the preview", approval(), native, withStreamed(state(t0)), []Kind{KindUpdate}},
		{"native ask_user goes to the preview", askUser(), native, withStreamed(state(t0)), []Kind{KindUpdate}},
		{"approval needs the approval capability", approval(), inputOnly, state(t0), []Kind{KindSend}},
		{"ask_user needs the input capability", askUser(), approvalOnly, state(t0), []Kind{KindSend}},
		{"plain approval without preview text is a message", approval(), noCaps, state(t0), []Kind{KindSend}},
		{"plain ask_user without preview text is a message", askUser(), noCaps, state(t0), []Kind{KindSend}},
		{"plain approval after preview text splits the preview", approval(), noCaps, withStreamed(state(t0)), []Kind{KindEndPreview, KindSend, KindOpenPreview}},
		{"plain ask_user after preview text splits the preview", askUser(), noCaps, withStreamed(state(t0)), []Kind{KindEndPreview, KindSend, KindOpenPreview}},
		{"approval is never hidden", approval(), noCaps, hiding(state(t0)), []Kind{KindSend}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actions, _ := Plan(tc.ev, tc.caps, tc.start)
			if !sameKinds(actions, tc.want...) {
				t.Fatalf("actions = %v, want %v", kinds(actions), tc.want)
			}
		})
	}

	t.Run("the prompt is a plain message that quotes the source message", func(t *testing.T) {
		actions, _ := Plan(approval(), noCaps, state(t0))
		msg := actions[0].Message
		if msg.Target != "chat-1" || msg.Message.Format != channel.MessageFormatPlain || msg.Message.Reply != reply {
			t.Fatalf("prompt = %+v", msg)
		}
		for _, want := range []string{"/approve 7", "/reject 7"} {
			if !strings.Contains(msg.Message.Text, want) {
				t.Fatalf("prompt missing %q:\n%s", want, msg.Message.Text)
			}
		}
		if actions[0].Event.ToolCall == nil || actions[0].Event.ToolCall.ApprovalID != "approval-1" {
			t.Fatalf("observers see %+v, want the approval event", actions[0].Event)
		}
	})
	t.Run("the ask_user prompt shows the question and its options", func(t *testing.T) {
		actions, _ := Plan(askUser(), noCaps, state(t0))
		text := actions[0].Message.Message.Text
		for _, want := range []string{"Pick one", "1) A"} {
			if !strings.Contains(text, want) {
				t.Fatalf("prompt missing %q:\n%s", want, text)
			}
		}
	})
	t.Run("the split preview ends with a flush, and the new preview has no text", func(t *testing.T) {
		actions, st := Plan(approval(), noCaps, withStreamed(state(t0)))
		if actions[0].Final == nil || actions[0].Final.Type != channel.StreamEventFinal || !actions[0].Final.Final.Message.IsEmpty() {
			t.Fatalf("split final = %+v", actions[0].Final)
		}
		next, _ := Plan(channel.StreamEvent{Type: channel.StreamEventError, Error: "boom"}, noCaps, st)
		if !sameKinds(next, KindUpdate) {
			t.Fatalf("error on the new preview = %v, want the preview to end with it", kinds(next))
		}
	})
}

// A channel that shows no preview gets every reply as messages.
func TestPlanWithoutPreview(t *testing.T) {
	caps := channel.ChannelCapabilities{Preview: channel.PreviewNone, NativeApproval: true}
	delta := channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "partial", Phase: channel.StreamPhaseText}
	attach := channel.StreamEvent{Type: channel.StreamEventAttachment, Attachments: []channel.Attachment{{Type: "image"}, {Type: "file"}}}

	t.Run("text and status are seen by observers only", func(t *testing.T) {
		actions, st := Plan(delta, caps, state(t0))
		if !sameKinds(actions, KindObserve) || st.streamed {
			t.Fatalf("actions = %v streamed = %v", kinds(actions), st.streamed)
		}
	})
	t.Run("attachments are sent one by one", func(t *testing.T) {
		actions, _ := Plan(attach, caps, state(t0))
		if !sameKinds(actions, KindAttach) || len(actions[0].Attachments) != 2 {
			t.Fatalf("actions = %+v", actions)
		}
	})
	t.Run("errors are messages", func(t *testing.T) {
		actions, _ := Plan(channel.StreamEvent{Type: channel.StreamEventError, Error: "boom"}, caps, state(t0))
		if !sameKinds(actions, KindSend) {
			t.Fatalf("actions = %v", kinds(actions))
		}
	})
	t.Run("approval is a message even when the channel renders it natively", func(t *testing.T) {
		actions, _ := Plan(approval(), caps, state(t0))
		if !sameKinds(actions, KindSend) {
			t.Fatalf("actions = %v", kinds(actions))
		}
	})
	t.Run("every output is a message", func(t *testing.T) {
		actions, _ := PlanEnd(caps, state(t0), []channel.Message{{Text: "first"}, {Text: "second"}}, false)
		if !sameKinds(actions, KindSend, KindSend) {
			t.Fatalf("actions = %v", kinds(actions))
		}
	})
}

// Attachments reach a preview that is not legacy as messages of their own.
func TestPlanAttachmentsByPreviewMode(t *testing.T) {
	attach := channel.StreamEvent{Type: channel.StreamEventAttachment, Attachments: []channel.Attachment{{Type: "image"}}}
	for mode, want := range map[channel.PreviewMode]Kind{
		channel.PreviewLegacy:       KindUpdate,
		channel.PreviewEditInPlace:  KindAttach,
		channel.PreviewNativeStream: KindAttach,
		channel.PreviewDraft:        KindAttach,
	} {
		actions, _ := Plan(attach, channel.ChannelCapabilities{Preview: mode}, state(t0))
		if !sameKinds(actions, want) {
			t.Errorf("preview mode %d: actions = %v, want %v", mode, kinds(actions), want)
		}
	}
}

// Contract: the end of a run finishes the preview once, sends each later
// output as a new message, and flushes a suppressed run's preview.
func TestPlanEndActions(t *testing.T) {
	first := channel.Message{Text: "first"}
	second := channel.Message{Text: "second"}
	failure := channel.StreamEvent{Type: channel.StreamEventError, Error: "failed", ErrorCode: "runtime_run_failed"}

	t.Run("no output finishes without a final", func(t *testing.T) {
		actions, _ := PlanEnd(noCaps, state(t0), nil, false)
		if !sameKinds(actions, KindFinish) || actions[0].Final != nil {
			t.Fatalf("actions = %+v", actions)
		}
	})
	t.Run("first output is the preview's final, later outputs are new messages", func(t *testing.T) {
		actions, _ := PlanEnd(noCaps, state(t0), []channel.Message{first, second}, false)
		if !sameKinds(actions, KindFinish, KindSend) {
			t.Fatalf("actions = %v", kinds(actions))
		}
		if actions[0].Final.Final.Message.Text != "first" || actions[1].Message.Message.Text != "second" {
			t.Fatalf("finals = %+v, sends = %+v", actions[0].Final, actions[1].Message)
		}
		if actions[1].Event.Type != channel.StreamEventFinal || actions[1].Event.Final.Message.Text != "second" {
			t.Fatalf("observers see %+v, want the second output as a final", actions[1].Event)
		}
	})
	t.Run("suppressed run flushes the text the preview shows and sends nothing", func(t *testing.T) {
		actions, _ := PlanEnd(noCaps, withStreamed(state(t0)), []channel.Message{first}, true)
		if !sameKinds(actions, KindFinish) || actions[0].Final == nil || actions[0].Final.Type != channel.StreamEventFinal {
			t.Fatalf("actions = %+v", actions)
		}
	})
	t.Run("suppressed run without preview text finishes without a final", func(t *testing.T) {
		actions, _ := PlanEnd(noCaps, state(t0), []channel.Message{first}, true)
		if !sameKinds(actions, KindFinish) || actions[0].Final != nil {
			t.Fatalf("actions = %+v", actions)
		}
	})
	t.Run("failure reply precedes the finish", func(t *testing.T) {
		actions, _ := PlanEnd(noCaps, withStreamed(SetFailure(state(t0), failure)), nil, false)
		if !sameKinds(actions, KindSend, KindFinish) {
			t.Fatalf("actions = %v", kinds(actions))
		}
	})
	t.Run("held errors are released before the failure reply", func(t *testing.T) {
		held := channel.StreamEvent{Type: channel.StreamEventError, Error: "held"}
		_, st := Plan(held, noCaps, withStreamed(holding(state(t0))))
		actions, _ := PlanErrors(noCaps, SetFailure(st, failure))
		if !sameKinds(actions, KindSend, KindSend) ||
			!strings.Contains(actions[0].Message.Message.Text, "held") || actions[1].Message.Message.Text != "failed" {
			t.Fatalf("actions = %+v", actions)
		}
	})
	t.Run("errors alone leave the preview to the caller", func(t *testing.T) {
		actions, _ := PlanErrors(noCaps, SetFailure(state(t0), failure))
		if !sameKinds(actions, KindUpdate) {
			t.Fatalf("actions = %v", kinds(actions))
		}
	})
}

// Contract for the reply window: inside it the reply is sent as is; after it,
// a proactive send drops the reply reference; after it without proactive send
// the run fails with the window's code.
func TestPlanReplyWindow(t *testing.T) {
	failure := channel.StreamEvent{Type: channel.StreamEventError, Error: "boom"}
	withText := withStreamed(state(t0))

	t.Run("inside the window the reply keeps its reference", func(t *testing.T) {
		st := withText
		st.Now = t0.Add(time.Minute)
		actions, _ := PlanEnd(winCaps, SetFailure(st, failure), []channel.Message{{Text: "answer"}}, false)
		if !sameKinds(actions, KindSend, KindFinish) || actions[0].Message.Message.Reply != reply {
			t.Fatalf("actions = %+v", actions)
		}
		if got := actions[1].Final.Final.Message.Reply; got != reply {
			t.Fatalf("final reply = %+v, want the source message", got)
		}
	})
	t.Run("after the window a proactive send drops the reply reference", func(t *testing.T) {
		caps := winCaps
		caps.ProactiveSend = true
		st := withText
		st.Now = t0.Add(2 * time.Hour)
		actions, _ := PlanEnd(caps, st, []channel.Message{{Text: "later", Reply: reply}, {Text: "again", Reply: reply}}, false)
		if !sameKinds(actions, KindFinish, KindSend) || actions[1].Message.Message.Reply != nil || actions[0].Final.Final.Message.Reply != nil {
			t.Fatalf("actions = %+v", actions)
		}
	})
	t.Run("after the window without proactive send the run fails with the window code", func(t *testing.T) {
		st := withText
		st.Now = t0.Add(2 * time.Hour)
		actions, next := Plan(channel.StreamEvent{Type: channel.StreamEventError, Error: "boom"}, winCaps, st)
		if !sameKinds(actions, KindFail) || actions[0].Code != apperror.CodeChannelReplyWindowExpired {
			t.Fatalf("actions = %+v", actions)
		}
		later, _ := Plan(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "x"}, winCaps, next)
		if len(later) != 0 {
			t.Fatalf("a failed run planned %v", kinds(later))
		}
	})
	t.Run("a run that outlasts the window fails at its end", func(t *testing.T) {
		st := state(t0)
		st.Now = t0.Add(2 * time.Hour)
		actions, _ := PlanEnd(winCaps, st, []channel.Message{{Text: "answer"}}, false)
		if !sameKinds(actions, KindFail) || actions[0].Code != apperror.CodeChannelReplyWindowExpired {
			t.Fatalf("actions = %+v", actions)
		}
	})
}
