package delivery

import (
	"strings"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/markdownmedia"
	"github.com/felinics/memoh/internal/redact"
)

var (
	t0    = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	reply = &channel.ReplyRef{Target: "chat-1", MessageID: "m-1"}
	// previewCaps declares a channel that shows a reply while it is produced.
	previewCaps = channel.ChannelCapabilities{Preview: channel.PreviewEditInPlace}
	winCaps     = channel.ChannelCapabilities{Preview: channel.PreviewEditInPlace, ReplyWindow: time.Hour}
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

func withPending(st State, text string) State {
	st.pending = text
	return st
}

func withDelivered(st State, text string) State {
	st.delivered = text
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
		{"attachment is a message of its own", attach, state(t0), []Kind{KindAttach}},
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
			actions, _ := Plan(tc.ev, previewCaps, tc.start)
			if !sameKinds(actions, tc.want...) {
				t.Fatalf("actions = %v, want %v", kinds(actions), tc.want)
			}
		})
	}

	t.Run("reasoning text is not preview text", func(t *testing.T) {
		_, st := Plan(thinking, previewCaps, state(t0))
		actions, _ := Plan(errEv, previewCaps, st)
		if !sameKinds(actions, KindUpdate) {
			t.Fatalf("error after reasoning = %v, want the preview to end with it", kinds(actions))
		}
	})
	t.Run("visible text makes a later error a message", func(t *testing.T) {
		_, st := Plan(delta, previewCaps, state(t0))
		actions, _ := Plan(errEv, previewCaps, st)
		if !sameKinds(actions, KindSend) {
			t.Fatalf("error after text = %v, want a message", kinds(actions))
		}
	})
	t.Run("a reset discards the text the preview shows", func(t *testing.T) {
		_, st := Plan(delta, previewCaps, state(t0))
		_, st = Plan(reset, previewCaps, st)
		actions, _ := Plan(errEv, previewCaps, st)
		if !sameKinds(actions, KindUpdate) {
			t.Fatalf("error after reset = %v, want the preview to end with it", kinds(actions))
		}
	})
	t.Run("an error is a message that quotes the source message", func(t *testing.T) {
		actions, _ := Plan(errEv, previewCaps, withStreamed(state(t0)))
		got := actions[0].Message
		if got.Target != "chat-1" || got.Message.Text != "Error: boom" || got.Message.Reply != reply {
			t.Fatalf("error message = %+v", got)
		}
		if actions[0].Event.Type != channel.StreamEventError {
			t.Fatalf("observers see %+v, want the error event", actions[0].Event)
		}
	})
	t.Run("reactions are addressed to the source message", func(t *testing.T) {
		actions, _ := Plan(react, previewCaps, state(t0))
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
	native := channel.ChannelCapabilities{Preview: channel.PreviewEditInPlace, NativeApproval: true, NativeUserInput: true}
	approvalOnly := channel.ChannelCapabilities{Preview: channel.PreviewEditInPlace, NativeApproval: true}
	inputOnly := channel.ChannelCapabilities{Preview: channel.PreviewEditInPlace, NativeUserInput: true}

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
		{"plain approval without preview text is a message", approval(), previewCaps, state(t0), []Kind{KindSend}},
		{"plain ask_user without preview text is a message", askUser(), previewCaps, state(t0), []Kind{KindSend}},
		{"plain approval after preview text splits the preview", approval(), previewCaps, withStreamed(state(t0)), []Kind{KindEndPreview, KindSend, KindOpenPreview}},
		{"plain ask_user after preview text splits the preview", askUser(), previewCaps, withStreamed(state(t0)), []Kind{KindEndPreview, KindSend, KindOpenPreview}},
		{"approval is never hidden", approval(), previewCaps, hiding(state(t0)), []Kind{KindSend}},
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
		actions, _ := Plan(approval(), previewCaps, state(t0))
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
		actions, _ := Plan(askUser(), previewCaps, state(t0))
		text := actions[0].Message.Message.Text
		for _, want := range []string{"Pick one", "1) A"} {
			if !strings.Contains(text, want) {
				t.Fatalf("prompt missing %q:\n%s", want, text)
			}
		}
	})
	t.Run("the split preview ends with a flush, and the new preview has no text", func(t *testing.T) {
		actions, st := Plan(approval(), previewCaps, withStreamed(state(t0)))
		if actions[0].Final == nil || actions[0].Final.Type != channel.StreamEventFinal || !actions[0].Final.Final.Message.IsEmpty() {
			t.Fatalf("split final = %+v", actions[0].Final)
		}
		next, _ := Plan(channel.StreamEvent{Type: channel.StreamEventError, Error: "boom"}, previewCaps, st)
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
	t.Run("text is held for the end of the run", func(t *testing.T) {
		_, st := Plan(delta, caps, state(t0))
		_, st = Plan(delta, caps, st)
		if st.pending != "partialpartial" {
			t.Fatalf("pending = %q", st.pending)
		}
		_, st = Plan(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "hmm", Phase: channel.StreamPhaseReasoning}, caps, st)
		if st.pending != "partialpartial" {
			t.Fatalf("reasoning joined the pending text: %q", st.pending)
		}
	})
}

// A channel without a preview gets the state of a finished tool call as a
// message of its own, unless tool calls are hidden.
func TestPlanToolResultsWithoutPreview(t *testing.T) {
	caps := channel.ChannelCapabilities{Preview: channel.PreviewNone}

	t.Run("a finished tool call is a plain message that quotes the source message", func(t *testing.T) {
		actions, _ := Plan(toolEnd(), caps, state(t0))
		if !sameKinds(actions, KindSend) {
			t.Fatalf("actions = %v", kinds(actions))
		}
		msg := actions[0].Message
		if msg.Target != "chat-1" || msg.Message.Format != channel.MessageFormatPlain || msg.Message.Reply != reply ||
			msg.Message.Text != "⚙️ search · completed" {
			t.Fatalf("tool message = %+v", msg)
		}
		if actions[0].Event.Type != channel.StreamEventToolCallEnd {
			t.Fatalf("observers see %+v, want the tool event", actions[0].Event)
		}
	})
	t.Run("the result carries what the tool returned", func(t *testing.T) {
		ev := channel.StreamEvent{Type: channel.StreamEventToolCallEnd, ToolCall: &channel.StreamToolCall{
			Name: "web_search", CallID: "c", Input: map[string]any{"query": "memoh"}, Result: map[string]any{"ok": false, "error": "no network"},
		}}
		actions, _ := Plan(ev, caps, state(t0))
		text := actions[0].Message.Message.Text
		if !strings.Contains(text, "web_search · failed") || !strings.Contains(text, "memoh") {
			t.Fatalf("tool message = %q", text)
		}
	})
	t.Run("a starting tool call is seen by observers only", func(t *testing.T) {
		actions, _ := Plan(toolStart(), caps, state(t0))
		if !sameKinds(actions, KindObserve) {
			t.Fatalf("actions = %v", kinds(actions))
		}
	})
	t.Run("hidden tool calls are seen by observers only", func(t *testing.T) {
		actions, _ := Plan(toolEnd(), caps, hiding(state(t0)))
		if !sameKinds(actions, KindObserve) {
			t.Fatalf("actions = %v", kinds(actions))
		}
	})
	t.Run("a tool call that renders to nothing is not sent", func(t *testing.T) {
		actions, _ := Plan(channel.StreamEvent{Type: channel.StreamEventToolCallEnd}, caps, state(t0))
		if !sameKinds(actions, KindObserve) {
			t.Fatalf("actions = %v", kinds(actions))
		}
	})
	t.Run("a preview shows the tool call itself", func(t *testing.T) {
		actions, _ := Plan(toolEnd(), channel.ChannelCapabilities{Preview: channel.PreviewEditInPlace}, state(t0))
		if !sameKinds(actions, KindUpdate) {
			t.Fatalf("actions = %v", kinds(actions))
		}
	})
	t.Run("after the window a proactive send drops the reference", func(t *testing.T) {
		proactive := channel.ChannelCapabilities{Preview: channel.PreviewNone, ReplyWindow: time.Hour, ProactiveSend: true}
		st := state(t0)
		st.Now = t0.Add(2 * time.Hour)
		actions, _ := Plan(toolEnd(), proactive, st)
		if !sameKinds(actions, KindSend) || actions[0].Message.Message.Reply != nil {
			t.Fatalf("actions = %+v", actions)
		}
	})
}

// A channel without a preview sends the text before an attachment ahead of it,
// so the attachment does not overtake the text that came first.
func TestPlanAttachmentsKeepTheOrderWithoutPreview(t *testing.T) {
	caps := channel.ChannelCapabilities{Preview: channel.PreviewNone}
	attach := channel.StreamEvent{Type: channel.StreamEventAttachment, Attachments: []channel.Attachment{{Type: "image"}}}

	t.Run("the held text is sent first, as a message that quotes the source message", func(t *testing.T) {
		actions, st := Plan(attach, caps, withPending(state(t0), " see below \n"))
		if !sameKinds(actions, KindSend, KindAttach) {
			t.Fatalf("actions = %v", kinds(actions))
		}
		if msg := actions[0].Message; msg.Target != "chat-1" || msg.Message.Text != "see below" || msg.Message.Reply != reply {
			t.Fatalf("text message = %+v", msg)
		}
		if actions[0].Event.Type != "" {
			t.Fatalf("observers already saw the text as deltas, got %+v", actions[0].Event)
		}
		if st.pending != "" || st.delivered != " see below \n" {
			t.Fatalf("state after the attachment: pending = %q, delivered = %q", st.pending, st.delivered)
		}
	})
	t.Run("text that follows is held again", func(t *testing.T) {
		_, st := Plan(attach, caps, withPending(state(t0), "first"))
		_, st = Plan(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "second", Phase: channel.StreamPhaseText}, caps, st)
		actions, st := Plan(attach, caps, st)
		if !sameKinds(actions, KindSend, KindAttach) || actions[0].Message.Message.Text != "second" || st.delivered != "firstsecond" {
			t.Fatalf("actions = %+v, delivered = %q", actions, st.delivered)
		}
	})
	t.Run("without held text the attachment goes alone", func(t *testing.T) {
		for _, held := range []string{"", " \n"} {
			actions, st := Plan(attach, caps, withPending(state(t0), held))
			if !sameKinds(actions, KindAttach) || st.pending != "" || st.delivered != "" {
				t.Fatalf("held %q: actions = %v, pending = %q, delivered = %q", held, kinds(actions), st.pending, st.delivered)
			}
		}
	})
	t.Run("text before a tool call is not sent", func(t *testing.T) {
		_, st := Plan(toolStart(), caps, withPending(state(t0), "let me look"))
		if st.pending != "" {
			t.Fatalf("pending = %q", st.pending)
		}
		hidden, st := Plan(toolStart(), caps, withPending(hiding(state(t0)), "let me look"))
		if !sameKinds(hidden, KindObserve) || st.pending != "" {
			t.Fatalf("hidden tool call: actions = %v, pending = %q", kinds(hidden), st.pending)
		}
		actions, _ := Plan(attach, caps, st)
		if !sameKinds(actions, KindAttach) {
			t.Fatalf("actions = %v", kinds(actions))
		}
	})
	t.Run("a retry discards held and delivered text", func(t *testing.T) {
		_, st := Plan(channel.StreamEvent{Type: channel.StreamEventReset}, caps, withDelivered(withPending(state(t0), "first try"), "earlier"))
		if st.pending != "" || st.delivered != "" {
			t.Fatalf("pending = %q, delivered = %q", st.pending, st.delivered)
		}
	})
	t.Run("a channel with a preview keeps the text in the preview", func(t *testing.T) {
		preview := channel.ChannelCapabilities{Preview: channel.PreviewEditInPlace}
		_, st := Plan(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "text", Phase: channel.StreamPhaseText}, preview, state(t0))
		actions, st := Plan(attach, preview, st)
		if !sameKinds(actions, KindAttach) || st.pending != "" || st.delivered != "" {
			t.Fatalf("actions = %v, pending = %q, delivered = %q", kinds(actions), st.pending, st.delivered)
		}
	})
}

// The end of a run does not send again the text that went out ahead of an
// attachment.
func TestPlanEndSkipsTextDeliveredBeforeAttachments(t *testing.T) {
	caps := channel.ChannelCapabilities{Preview: channel.PreviewNone}
	cases := []struct {
		name      string
		delivered string
		outputs   []string
		want      []Kind
		sent      []string
	}{
		{"the rest of the output follows the delivered text", "see ", []string{"see below"}, []Kind{KindSend}, []string{"below"}},
		{"an output delivered whole is only observed", "all of it", []string{"all of it"}, []Kind{KindObserve}, []string{""}},
		{"delivered text can span outputs", "one two", []string{"one", "two three"}, []Kind{KindObserve, KindSend}, []string{"", "three"}},
		{"text that does not continue from it is sent whole", "other", []string{"new text"}, []Kind{KindSend}, []string{"new text"}},
		{"nothing delivered leaves the outputs alone", "", []string{"a", "b"}, []Kind{KindSend, KindSend}, []string{"a", "b"}},
		{"matching stops at the first output that does not continue", "start", []string{"unrelated", "start again"}, []Kind{KindSend, KindSend}, []string{"unrelated", "start again"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outputs := make([]channel.Message, 0, len(tc.outputs))
			for _, text := range tc.outputs {
				outputs = append(outputs, channel.Message{Text: text})
			}
			actions, _ := PlanEnd(caps, withDelivered(state(t0), tc.delivered), outputs, false)
			if !sameKinds(actions, tc.want...) {
				t.Fatalf("actions = %v, want %v", kinds(actions), tc.want)
			}
			for i, a := range actions {
				if a.Kind == KindSend && a.Message.Message.Text != tc.sent[i] {
					t.Errorf("action %d sends %q, want %q", i, a.Message.Message.Text, tc.sent[i])
				}
				// Observers get each output whole.
				if got := a.Event.Final.Message.Text; got != tc.outputs[i] {
					t.Errorf("action %d is observed as %q, want %q", i, got, tc.outputs[i])
				}
			}
		})
	}
}

// A channel that declares no preview mode shows no preview: its reply is held
// until the run ends and sent once.
func TestUndeclaredPreviewShowsNone(t *testing.T) {
	var caps channel.ChannelCapabilities
	if caps.Preview != channel.PreviewNone {
		t.Fatalf("zero preview mode = %d, want PreviewNone", caps.Preview)
	}
	delta := channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "partial", Phase: channel.StreamPhaseText}
	actions, st := Plan(delta, caps, state(t0))
	if !sameKinds(actions, KindObserve) {
		t.Fatalf("delta: actions = %v, want the observers only", kinds(actions))
	}
	ended, _ := PlanEnd(caps, st, []channel.Message{{Text: "partial"}}, false)
	if !sameKinds(ended, KindSend) {
		t.Fatalf("end: actions = %v, want one message", kinds(ended))
	}
}

// Attachments reach every preview mode as messages of their own.
func TestPlanAttachmentsByPreviewMode(t *testing.T) {
	attach := channel.StreamEvent{Type: channel.StreamEventAttachment, Attachments: []channel.Attachment{{Type: "image"}}}
	for mode, want := range map[channel.PreviewMode]Kind{
		channel.PreviewNone:         KindAttach,
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
		actions, _ := PlanEnd(previewCaps, state(t0), nil, false)
		if !sameKinds(actions, KindFinish) || actions[0].Final != nil {
			t.Fatalf("actions = %+v", actions)
		}
	})
	t.Run("first output is the preview's final, later outputs are new messages", func(t *testing.T) {
		actions, _ := PlanEnd(previewCaps, state(t0), []channel.Message{first, second}, false)
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
		actions, _ := PlanEnd(previewCaps, withStreamed(state(t0)), []channel.Message{first}, true)
		if !sameKinds(actions, KindFinish) || actions[0].Final == nil || actions[0].Final.Type != channel.StreamEventFinal {
			t.Fatalf("actions = %+v", actions)
		}
	})
	t.Run("suppressed run without preview text finishes without a final", func(t *testing.T) {
		actions, _ := PlanEnd(previewCaps, state(t0), []channel.Message{first}, true)
		if !sameKinds(actions, KindFinish) || actions[0].Final != nil {
			t.Fatalf("actions = %+v", actions)
		}
	})
	t.Run("failure reply precedes the finish", func(t *testing.T) {
		actions, _ := PlanEnd(previewCaps, withStreamed(SetFailure(state(t0), failure)), nil, false)
		if !sameKinds(actions, KindSend, KindFinish) {
			t.Fatalf("actions = %v", kinds(actions))
		}
	})
	t.Run("held errors are released before the failure reply", func(t *testing.T) {
		held := channel.StreamEvent{Type: channel.StreamEventError, Error: "held"}
		_, st := Plan(held, previewCaps, withStreamed(holding(state(t0))))
		actions, _ := PlanErrors(previewCaps, SetFailure(st, failure))
		if !sameKinds(actions, KindSend, KindSend) ||
			!strings.Contains(actions[0].Message.Message.Text, "held") || actions[1].Message.Message.Text != "failed" {
			t.Fatalf("actions = %+v", actions)
		}
	})
	t.Run("errors alone leave the preview to the caller", func(t *testing.T) {
		actions, _ := PlanErrors(previewCaps, SetFailure(state(t0), failure))
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
	t.Run("attachments follow the same rule as messages", func(t *testing.T) {
		attach := channel.StreamEvent{Type: channel.StreamEventAttachment, Attachments: []channel.Attachment{{Type: "image"}}}
		caps := winCaps
		caps.Preview = channel.PreviewEditInPlace
		st := state(t0)
		st.Now = t0.Add(time.Minute)
		actions, _ := Plan(attach, caps, st)
		if !sameKinds(actions, KindAttach) || actions[0].Reply != reply {
			t.Fatalf("inside the window: actions = %+v, want the source message quoted", actions)
		}
		caps.ProactiveSend = true
		st.Now = t0.Add(2 * time.Hour)
		actions, _ = Plan(attach, caps, st)
		if !sameKinds(actions, KindAttach) || actions[0].Reply != nil {
			t.Fatalf("after the window: actions = %+v, want no reference", actions)
		}
		caps.ProactiveSend = false
		actions, _ = Plan(attach, caps, st)
		if !sameKinds(actions, KindFail) {
			t.Fatalf("after the window without proactive send: actions = %v", kinds(actions))
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

// Error text is masked wherever it leaves Plan: in the event a preview gets and
// in the message that carries it.
func TestPlanMasksSecretsInErrors(t *testing.T) {
	const secret = "sk-delivery-0123456789abcdef"
	redact.SetSecrets("delivery-plan-test", secret)
	t.Cleanup(func() { redact.SetSecrets("delivery-plan-test") })
	masked := "rejected " + strings.Repeat("*", len(secret))
	failure := channel.StreamEvent{Type: channel.StreamEventError, Error: "rejected " + secret}
	coded := channel.StreamEvent{Type: channel.StreamEventError, Error: "rejected " + secret, ErrorCode: "runtime_run_failed"}
	none := channel.ChannelCapabilities{Preview: channel.PreviewNone}

	t.Run("an error the preview ends with", func(t *testing.T) {
		actions, _ := Plan(failure, previewCaps, state(t0))
		if !sameKinds(actions, KindUpdate) || actions[0].Event.Error != masked {
			t.Fatalf("actions = %+v, want the preview to get %q", actions, masked)
		}
	})
	t.Run("an error message after preview text", func(t *testing.T) {
		actions, _ := Plan(failure, previewCaps, withStreamed(state(t0)))
		if !sameKinds(actions, KindSend) || actions[0].Message.Message.Text != "Error: "+masked || actions[0].Event.Error != masked {
			t.Fatalf("actions = %+v", actions)
		}
	})
	t.Run("an error message on a channel without a preview", func(t *testing.T) {
		actions, _ := Plan(failure, none, state(t0))
		if !sameKinds(actions, KindSend) || actions[0].Message.Message.Text != "Error: "+masked {
			t.Fatalf("actions = %+v", actions)
		}
	})
	t.Run("an error that carries a code", func(t *testing.T) {
		actions, _ := Plan(coded, none, state(t0))
		if !sameKinds(actions, KindSend) || actions[0].Message.Message.Text != masked {
			t.Fatalf("actions = %+v", actions)
		}
	})
	t.Run("an error held for the end of the run", func(t *testing.T) {
		_, st := Plan(failure, none, holding(state(t0)))
		actions, _ := PlanErrors(none, st)
		if !sameKinds(actions, KindSend) || actions[0].Message.Message.Text != "Error: "+masked {
			t.Fatalf("actions = %+v", actions)
		}
	})
	t.Run("the failure reply of a run", func(t *testing.T) {
		actions, _ := PlanErrors(previewCaps, SetFailure(state(t0), failure))
		if !sameKinds(actions, KindUpdate) || actions[0].Event.Error != masked {
			t.Fatalf("actions = %+v", actions)
		}
		actions, _ = PlanErrors(none, SetFailure(state(t0), coded))
		if !sameKinds(actions, KindSend) || actions[0].Message.Message.Text != masked {
			t.Fatalf("actions = %+v", actions)
		}
	})
	t.Run("text without a secret is left as it is", func(t *testing.T) {
		actions, _ := Plan(channel.StreamEvent{Type: channel.StreamEventError, Error: "boom"}, none, state(t0))
		if actions[0].Message.Message.Text != "Error: boom" {
			t.Fatalf("actions = %+v", actions)
		}
	})
}

// A tool call the channel shows ends the text before it the same way: the text
// is a message of its own, and the call opens the next preview.
func TestPlanShownToolCallEndsThePreviewSegment(t *testing.T) {
	text := channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "Let me check.", Phase: channel.StreamPhaseText}
	_, st := Plan(text, previewCaps, state(t0))
	actions, st := Plan(toolStart(), previewCaps, st)
	if !sameKinds(actions, KindEndPreview, KindOpenPreview, KindUpdate) {
		t.Fatalf("actions = %v", kinds(actions))
	}
	if actions[2].Event.Type != channel.StreamEventToolCallStart {
		t.Fatalf("the call is shown by the preview that opens, got %+v", actions[2].Event)
	}
	if len(st.committed) != 1 || st.committed[0] != "Let me check." || st.streamed {
		t.Fatalf("committed = %q streamed = %v", st.committed, st.streamed)
	}
	actions, _ = Plan(toolStart(), previewCaps, state(t0))
	if !sameKinds(actions, KindUpdate) {
		t.Fatalf("a call with no text before it: actions = %v", kinds(actions))
	}
}

// A tool call the channel hides is a boundary the channel cannot show. The
// text the preview shows before it ends as a message of its own, and what
// follows opens a new preview, so the text is not overwritten by the run's
// final reply.
func TestPlanHiddenToolCallEndsThePreviewSegment(t *testing.T) {
	text := func(delta string) channel.StreamEvent {
		return channel.StreamEvent{Type: channel.StreamEventDelta, Delta: delta, Phase: channel.StreamPhaseText}
	}

	t.Run("the text before the call is ended and a new preview opens", func(t *testing.T) {
		for _, mode := range []channel.PreviewMode{channel.PreviewEditInPlace, channel.PreviewNativeStream, channel.PreviewDraft} {
			caps := channel.ChannelCapabilities{Preview: mode}
			_, st := Plan(text("Let me check."), caps, hiding(state(t0)))
			actions, st := Plan(toolStart(), caps, st)
			if !sameKinds(actions, KindObserve, KindEndPreview, KindOpenPreview) {
				t.Fatalf("mode %d: actions = %v", mode, kinds(actions))
			}
			if actions[1].Final == nil || actions[1].Final.Type != channel.StreamEventFinal || !actions[1].Final.Final.Message.IsEmpty() {
				t.Fatalf("mode %d: the open preview must end with a flush, got %+v", mode, actions[1].Final)
			}
			if len(st.committed) != 1 || st.committed[0] != "Let me check." {
				t.Fatalf("mode %d: committed = %q", mode, st.committed)
			}
			if st.streamed || st.segment != "" {
				t.Fatalf("mode %d: streamed = %v segment = %q", mode, st.streamed, st.segment)
			}
		}
	})
	t.Run("the call is still shown to observers", func(t *testing.T) {
		_, st := Plan(text("Let me check."), previewCaps, hiding(state(t0)))
		actions, _ := Plan(toolStart(), previewCaps, st)
		if actions[0].Event.Type != channel.StreamEventToolCallStart || actions[0].Event.ToolCall == nil || actions[0].Event.ToolCall.Name != "search" {
			t.Fatalf("observers see %+v, want the tool call", actions[0].Event)
		}
	})
	t.Run("the text after the call starts a fresh segment", func(t *testing.T) {
		_, st := Plan(text("Let me check."), previewCaps, hiding(state(t0)))
		_, st = Plan(toolStart(), previewCaps, st)
		_, st = Plan(text("Here is the answer."), previewCaps, st)
		if !st.streamed || st.segment != "Here is the answer." || len(st.committed) != 1 {
			t.Fatalf("streamed = %v segment = %q committed = %q", st.streamed, st.segment, st.committed)
		}
	})
	t.Run("no text streamed keeps the placeholder for what follows", func(t *testing.T) {
		actions, st := Plan(toolStart(), previewCaps, hiding(state(t0)))
		if !sameKinds(actions, KindObserve) || len(st.committed) != 0 {
			t.Fatalf("actions = %v committed = %q", kinds(actions), st.committed)
		}
	})
	t.Run("a channel without a preview is unchanged", func(t *testing.T) {
		none := channel.ChannelCapabilities{Preview: channel.PreviewNone}
		_, st := Plan(text("Let me check."), none, hiding(state(t0)))
		actions, st := Plan(toolStart(), none, st)
		if !sameKinds(actions, KindObserve) || len(st.committed) != 0 || st.segment != "" {
			t.Fatalf("actions = %v committed = %q segment = %q", kinds(actions), st.committed, st.segment)
		}
	})
	t.Run("reasoning text does not join the segment", func(t *testing.T) {
		_, st := Plan(text("Let me check."), previewCaps, hiding(state(t0)))
		_, st = Plan(channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "hmm", Phase: channel.StreamPhaseReasoning}, previewCaps, st)
		_, st = Plan(toolStart(), previewCaps, st)
		if len(st.committed) != 1 || st.committed[0] != "Let me check." {
			t.Fatalf("committed = %q", st.committed)
		}
	})
	t.Run("a retry discards the text before the call", func(t *testing.T) {
		_, st := Plan(text("first try"), previewCaps, hiding(state(t0)))
		_, st = Plan(channel.StreamEvent{Type: channel.StreamEventReset}, previewCaps, st)
		_, st = Plan(text("second try"), previewCaps, st)
		_, st = Plan(toolStart(), previewCaps, st)
		if len(st.committed) != 1 || st.committed[0] != "second try" {
			t.Fatalf("committed = %q", st.committed)
		}
	})
	t.Run("a prompt that splits the preview does not leave a segment behind", func(t *testing.T) {
		_, st := Plan(text("checking "), previewCaps, hiding(state(t0)))
		_, st = Plan(approval(), previewCaps, st)
		_, st = Plan(text("done"), previewCaps, st)
		_, st = Plan(toolStart(), previewCaps, st)
		if len(st.committed) != 1 || st.committed[0] != "done" {
			t.Fatalf("committed = %q", st.committed)
		}
	})
}

// The end of a run does not deliver again the text a hidden tool call already
// ended as a message of its own. A run that repeats it (external runtimes keep
// the text before a tool call as an assistant output of its own) sends the
// rest, not the repeat.
func TestPlanEndSkipsTextAHiddenToolCallCommitted(t *testing.T) {
	text := func(delta string) channel.StreamEvent {
		return channel.StreamEvent{Type: channel.StreamEventDelta, Delta: delta, Phase: channel.StreamPhaseText}
	}
	// afterBreak is the reply state once "Let me check." was ended by a hidden
	// tool call and "Here is the answer." has streamed into the new preview.
	afterBreak := func() State {
		_, st := Plan(text("Let me check."), previewCaps, hiding(state(t0)))
		_, st = Plan(toolStart(), previewCaps, st)
		_, st = Plan(text("Here is the answer."), previewCaps, st)
		return st
	}

	t.Run("the repeat is observed and the rest ends the preview", func(t *testing.T) {
		actions, _ := PlanEnd(previewCaps, afterBreak(), []channel.Message{{Text: "Let me check."}, {Text: "Here is the answer."}}, false)
		if !sameKinds(actions, KindObserve, KindFinish) {
			t.Fatalf("actions = %v", kinds(actions))
		}
		if actions[1].Final == nil || actions[1].Final.Final.Message.Text != "Here is the answer." {
			t.Fatalf("final = %+v", actions[1].Final)
		}
		if actions[0].Event.Final == nil || actions[0].Event.Final.Message.Text != "Let me check." {
			t.Fatalf("observers see %+v, want the repeated output whole", actions[0].Event)
		}
	})
	t.Run("a run that only repeats ends without a message", func(t *testing.T) {
		_, st := Plan(text("Let me check."), previewCaps, hiding(state(t0)))
		_, st = Plan(toolStart(), previewCaps, st)
		actions, _ := PlanEnd(previewCaps, st, []channel.Message{{Text: " Let me check. "}}, false)
		if !sameKinds(actions, KindObserve, KindFinish) {
			t.Fatalf("actions = %v", kinds(actions))
		}
		if actions[1].Final != nil {
			t.Fatalf("final = %+v, want the preview to end without a message", actions[1].Final)
		}
	})
	t.Run("a repeat does not discard text the new preview shows", func(t *testing.T) {
		actions, _ := PlanEnd(previewCaps, afterBreak(), []channel.Message{{Text: "Let me check."}}, false)
		if !sameKinds(actions, KindObserve, KindFinish) {
			t.Fatalf("actions = %v", kinds(actions))
		}
		if actions[1].Final == nil || !actions[1].Final.Final.Message.IsEmpty() {
			t.Fatalf("final = %+v, want a flush that keeps the streamed text", actions[1].Final)
		}
	})
	t.Run("a repeat among later outputs is skipped too", func(t *testing.T) {
		actions, _ := PlanEnd(previewCaps, afterBreak(), []channel.Message{
			{Text: "Let me check."},
			{Text: "Here is the answer."},
			{Text: "Let me check."},
		}, false)
		if !sameKinds(actions, KindObserve, KindFinish, KindObserve) {
			t.Fatalf("actions = %v", kinds(actions))
		}
	})
	t.Run("an output that carries more than the text is not a repeat", func(t *testing.T) {
		withAttachment := channel.Message{Text: "Let me check.", Attachments: []channel.Attachment{{Type: channel.AttachmentFile, URL: "https://example.test/a.txt"}}}
		actions, _ := PlanEnd(previewCaps, afterBreak(), []channel.Message{withAttachment}, false)
		if !sameKinds(actions, KindFinish) {
			t.Fatalf("actions = %v", kinds(actions))
		}
		if actions[0].Final == nil || len(actions[0].Final.Final.Message.Attachments) != 1 {
			t.Fatalf("final = %+v", actions[0].Final)
		}
	})
	t.Run("an output that publishes media is not a repeat", func(t *testing.T) {
		for _, key := range []string{markdownmedia.MetadataKey, markdownmedia.EarlierMetadataKey} {
			published := channel.Message{Text: "Let me check.", Metadata: map[string]any{key: []markdownmedia.Binding{}}}
			actions, _ := PlanEnd(previewCaps, afterBreak(), []channel.Message{published}, false)
			if !sameKinds(actions, KindFinish) || actions[0].Final == nil || actions[0].Final.Final.Message.Text != "Let me check." {
				t.Fatalf("%s: actions = %+v", key, actions)
			}
		}
	})
	t.Run("text that was never committed is delivered as usual", func(t *testing.T) {
		_, st := Plan(text("Here is the answer."), previewCaps, hiding(state(t0)))
		actions, _ := PlanEnd(previewCaps, st, []channel.Message{{Text: "Here is the answer."}}, false)
		if !sameKinds(actions, KindFinish) || actions[0].Final == nil || actions[0].Final.Final.Message.Text != "Here is the answer." {
			t.Fatalf("actions = %+v", actions)
		}
	})
	t.Run("a channel without a preview is unchanged", func(t *testing.T) {
		none := channel.ChannelCapabilities{Preview: channel.PreviewNone}
		_, st := Plan(text("Let me check."), none, hiding(state(t0)))
		_, st = Plan(toolStart(), none, st)
		actions, _ := PlanEnd(none, st, []channel.Message{{Text: "let me check"}, {Text: "the answer"}}, false)
		if !sameKinds(actions, KindSend, KindSend) {
			t.Fatalf("actions = %v", kinds(actions))
		}
	})
}
