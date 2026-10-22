// Package delivery maps one run's turn events onto a platform reply. Plan is
// the decision: a pure function from one event, the channel's capabilities and
// the reply state to the actions to perform. Reply performs them in order on a
// Surface.
package delivery

import (
	"context"
	"strings"
	"time"

	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/markdownmedia"
	"github.com/felinics/memoh/internal/redact"
)

// Surface is what a platform surface offers a reply. It does not decide what
// an event means. channel's manager implements it without importing this
// package, so the signatures use channel's own types.
type Surface interface {
	// Capabilities returns the capabilities as refined for target.
	Capabilities(target string) channel.ChannelCapabilities
	// Send delivers a whole message. Splitting it to the platform's size and
	// downgrading its format and buttons happen here, once, for every caller.
	Send(ctx context.Context, msg channel.OutboundMessage) error
	// Attach sends one attachment to target as a message of its own, quoting
	// reply when it is not nil.
	Attach(ctx context.Context, target string, att channel.Attachment, reply *channel.ReplyRef) error
	// React adds or removes a reaction.
	React(ctx context.Context, req channel.ReactRequest) error
	// OpenPreview opens the preview of a reply on target.
	OpenPreview(ctx context.Context, target string, opts channel.StreamOptions) (channel.Preview, error)
}

// Kind is the type of an Action.
type Kind int

const (
	// KindUpdate forwards Event to the preview.
	KindUpdate Kind = iota
	// KindObserve shows Event to observers only.
	KindObserve
	// KindEndPreview ends the preview with Final so that a message sent next
	// follows its text. KindOpenPreview opens the preview for what comes after.
	KindEndPreview
	// KindOpenPreview opens a preview that carries on a reply whose earlier
	// text is already delivered.
	KindOpenPreview
	// KindFinish ends the reply with Final, or without a final when Final is nil.
	KindFinish
	// KindSend sends Message as a standalone message.
	KindSend
	// KindAttach sends Attachments, each as a message of its own that quotes
	// Reply.
	KindAttach
	// KindReact applies Reactions.
	KindReact
	// KindFail ends the run with Code: the reply window closed and the
	// platform does not accept a proactive message.
	KindFail
)

// Action is one step of a reply. Only the fields of its Kind are set, and
// Event also names what observers see for the kinds that carry one.
type Action struct {
	Kind        Kind
	Event       channel.StreamEvent
	Final       *channel.StreamEvent
	Message     channel.OutboundMessage
	Attachments []channel.Attachment
	Reply       *channel.ReplyRef
	Reactions   []channel.ReactRequest
	Code        apperror.Code
}

// State is the reply state Plan carries from one event to the next.
type State struct {
	// Target and Reply address the standalone messages Plan sends.
	Target string
	Reply  *channel.ReplyRef
	// SourceMessageID is the message a reaction is put on.
	SourceMessageID string
	// ReceivedAt is when the inbound message arrived. L3 passes the original
	// time of a queued item here.
	ReceivedAt time.Time
	// Now is the time the decision is made.
	Now time.Time
	// HoldErrors keeps stream errors back until the run ends, for runs that
	// report a run_terminal event.
	HoldErrors bool
	// HideToolCalls keeps ordinary tool events from the preview. Observers
	// still see them.
	HideToolCalls bool

	held   []channel.StreamEvent
	failed bool
	// streamed says the open preview shows text. Before that, an error has no
	// preview text to overwrite and goes through the preview.
	streamed bool
	// proactive is set once the reply window has closed and the platform
	// accepts a message without a reply reference.
	proactive bool
	// failure is the terminal failure reply, rendered at the end of the run.
	failure *channel.StreamEvent
	// pending is the text of the assistant output in progress that a channel
	// without a preview has not sent yet. delivered is the text of this run that
	// was sent ahead of an attachment, so that the end of the run does not send
	// it again.
	pending   string
	delivered string
	// segment is the text the open preview shows that a tool call has not ended
	// yet. committed is the text of the segments tool calls already ended, each
	// of which is a message of its own by the time the run ends.
	segment   string
	committed []string
}

// Plan returns the actions for one event and the state that follows it. It
// does not perform them.
//
// Errors are never written over preview text. Before any text has shown, an
// error is the preview's terminal state, so no empty placeholder is left
// behind. Once the preview shows text, the error is a message of its own, so
// the text stays and the error is not throttled with it. With HoldErrors set,
// errors are kept until the run ends.
//
// A request for approval or an answer is handed to the preview when the
// channel renders it natively. Otherwise it is sent as a plain message: the
// preview that shows text ends first, so the prompt follows that text, and a
// new preview carries what comes after.
//
// A channel without a preview gets the state of each finished tool call as a
// message of its own, unless tool calls are hidden. It holds the reply text
// until the run ends, except that the text before an attachment is sent ahead
// of it.
//
// A hidden tool call is a boundary the channel cannot show: the text the
// preview shows before it ends as a message of its own, and what follows opens
// a new preview. A run that then repeats that text as one of its outputs does
// not deliver it twice.
func Plan(ev channel.StreamEvent, caps channel.ChannelCapabilities, st State) ([]Action, State) {
	if st.failed {
		return nil, st
	}
	switch ev.Type {
	case channel.StreamEventError:
		if st.HoldErrors {
			st.held = append(st.held, ev)
			return nil, st
		}
		return sendError(ev, caps, st)
	case channel.StreamEventReaction:
		if len(ev.Reactions) == 0 {
			return nil, st
		}
		return []Action{{Kind: KindReact, Reactions: st.reactions(ev.Reactions)}}, st
	}
	reach, next := windowOK(caps, st)
	if !reach {
		return []Action{{Kind: KindFail, Code: apperror.CodeChannelReplyWindowExpired}}, failed(next)
	}
	st = next
	if ev.Type == channel.StreamEventToolCallStart {
		// The assistant output that calls a tool is not a reply, so the text
		// before the call is not sent on a channel without a preview.
		st.pending = ""
	}
	switch {
	case ev.Type == channel.StreamEventToolCallStart && ev.ToolCall != nil && needsAnswer(ev.ToolCall):
		return planPrompt(ev, caps, st)
	case ev.Type == channel.StreamEventToolCallStart || ev.Type == channel.StreamEventToolCallEnd:
		if ev.Type == channel.StreamEventToolCallStart && caps.Preview != channel.PreviewNone && st.streamed {
			// The text before the call is not the reply's own text: it ends as a
			// message of its own, and what follows opens a new preview. The
			// call itself is shown after it, or only observed when the channel
			// hides tool calls.
			return planSegmentBreak(ev, st)
		}
		if st.HideToolCalls {
			return observe(ev), st
		}
		if ev.Type == channel.StreamEventToolCallEnd && caps.Preview == channel.PreviewNone {
			return planToolResult(ev, st)
		}
	case ev.Type == channel.StreamEventDelta:
		if visibleText(ev) {
			if caps.Preview == channel.PreviewNone {
				st.pending += ev.Delta
			} else {
				st.streamed = true
				st.segment += ev.Delta
			}
		}
	case ev.Type == channel.StreamEventReset:
		st.streamed = false
		st.pending, st.delivered = "", ""
		st.segment = ""
	case ev.Type == channel.StreamEventAttachment:
		return planAttach(ev, st)
	}
	if caps.Preview == channel.PreviewNone {
		return observe(ev), st
	}
	return []Action{{Kind: KindUpdate, Event: ev}}, st
}

// PlanErrors returns the actions that deliver the errors a run ends with: the
// held errors when no run_terminal event replaced them, then the failure reply.
func PlanErrors(caps channel.ChannelCapabilities, st State) ([]Action, State) {
	var actions []Action
	if st.failed {
		return nil, st
	}
	for _, held := range st.held {
		acts, next := sendError(held, caps, st)
		actions = append(actions, acts...)
		st = next
	}
	st.held = nil
	if st.failure != nil {
		acts, next := sendError(*st.failure, caps, st)
		actions = append(actions, acts...)
		st = next
	}
	return actions, st
}

// PlanEnd returns the actions that close a run that succeeded: its errors, if
// any, and the final messages. The first output is the preview's final; each
// later output is a new message. suppress marks a run whose replies were sent
// by a messaging tool: the text the preview shows is flushed, and no final is
// sent.
func PlanEnd(caps channel.ChannelCapabilities, st State, outputs []channel.Message, suppress bool) ([]Action, State) {
	actions, st := PlanErrors(caps, st)
	if st.failed {
		return actions, st
	}
	if suppress {
		return append(actions, finishAction(st)), st
	}
	if len(outputs) == 0 {
		return append(actions, Action{Kind: KindFinish}), st
	}
	reach, next := windowOK(caps, st)
	if !reach {
		return append(actions, Action{Kind: KindFail, Code: apperror.CodeChannelReplyWindowExpired}), failed(next)
	}
	st = next
	if caps.Preview != channel.PreviewNone {
		// The first output that does not repeat text the channel already
		// delivered ends the preview; a repeat is only observed, in the order
		// the run produced it.
		ended := false
		var tail []channel.Message
		for _, out := range outputs {
			switch {
			case st.repeatsCommitted(out):
				actions = append(actions, Action{Kind: KindObserve, Event: finalEvent(st.outbound(out))})
			case !ended:
				final := finalEvent(st.outbound(out))
				actions = append(actions, Action{Kind: KindFinish, Final: &final})
				ended = true
			default:
				tail = append(tail, out)
			}
		}
		if !ended {
			return append(actions, finishAction(st)), st
		}
		return append(actions, st.tailActions(tail)...), st
	}
	return append(actions, st.tailActions(outputs)...), st
}

// finishAction ends the preview. Text the preview shows is flushed first, so
// that text of its own is not lost; a preview that shows none is removed.
func finishAction(st State) Action {
	if !st.streamed {
		return Action{Kind: KindFinish}
	}
	flush := channel.StreamEvent{Type: channel.StreamEventFinal, Final: &channel.StreamFinalizePayload{}}
	return Action{Kind: KindFinish, Final: &flush}
}

// tailActions sends the outputs that follow the one which ended the preview.
func (st State) tailActions(outputs []channel.Message) []Action {
	actions := make([]Action, 0, len(outputs))
	delivered := strings.TrimSpace(st.delivered)
	for _, out := range outputs {
		whole := st.outbound(out)
		msg := whole
		msg.Message.Text, delivered = withoutDelivered(out.Text, delivered)
		kind := KindSend
		if msg.Message.IsEmpty() {
			kind = KindObserve
		}
		// Observers get the output whole, as they would from a preview's final.
		actions = append(actions, Action{Kind: kind, Message: msg, Event: finalEvent(whole)})
	}
	return actions
}

// repeatsCommitted reports whether an output only repeats text a hidden tool
// call already ended as a message of its own. Attachments, buttons and
// published media make an output more than that text, so they are never a repeat.
func (st State) repeatsCommitted(msg channel.Message) bool {
	if len(st.committed) == 0 || len(msg.Attachments) > 0 || len(msg.Actions) > 0 {
		return false
	}
	// An output that publishes media also delivers what the stream withheld; the
	// stream drops the repeated text itself.
	if _, ok := msg.Metadata[markdownmedia.MetadataKey]; ok {
		return false
	}
	if _, ok := msg.Metadata[markdownmedia.EarlierMetadataKey]; ok {
		return false
	}
	text := strings.TrimSpace(msg.PlainText())
	if text == "" {
		return false
	}
	for _, committed := range st.committed {
		if committed == text {
			return true
		}
	}
	return false
}

// withoutDelivered drops from text what was already sent ahead of an attachment.
// delivered is the text still to be matched, and the second result is what
// remains of it for the next output. Text that does not continue from delivered
// is kept whole: sending a line twice costs less than losing one.
func withoutDelivered(text, delivered string) (string, string) {
	whole := strings.TrimSpace(text)
	if delivered == "" || whole == "" {
		return text, delivered
	}
	if rest, ok := strings.CutPrefix(whole, delivered); ok {
		return strings.TrimSpace(rest), ""
	}
	if rest, ok := strings.CutPrefix(delivered, whole); ok {
		return "", strings.TrimSpace(rest)
	}
	return text, ""
}

// SetFailure records the terminal failure reply, sent when the run ends.
func SetFailure(st State, ev channel.StreamEvent) State {
	st.failure = &ev
	return st
}

// planSegmentBreak ends the text that a tool call interrupts. The preview that shows that text ends holding it, so the text
// stands as a message of its own, and a new preview carries what follows. The
// text is remembered so that a run repeating it at the end does not deliver it
// twice.
func planSegmentBreak(ev channel.StreamEvent, st State) ([]Action, State) {
	flush := channel.StreamEvent{Type: channel.StreamEventFinal, Final: &channel.StreamFinalizePayload{}}
	if text := strings.TrimSpace(st.segment); text != "" {
		st.committed = append(st.committed, text)
	}
	st.segment, st.streamed = "", false
	if st.HideToolCalls {
		return []Action{
			{Kind: KindObserve, Event: ev},
			{Kind: KindEndPreview, Final: &flush},
			{Kind: KindOpenPreview},
		}, st
	}
	return []Action{
		{Kind: KindEndPreview, Final: &flush},
		{Kind: KindOpenPreview},
		{Kind: KindUpdate, Event: ev},
	}, st
}

// planPrompt places a request for approval or an answer.
func planPrompt(ev channel.StreamEvent, caps channel.ChannelCapabilities, st State) ([]Action, State) {
	tc := ev.ToolCall
	native := (channel.HasUserInputAction(tc.Actions) && caps.NativeUserInput) ||
		(!channel.HasUserInputAction(tc.Actions) && caps.NativeApproval)
	if native && caps.Preview != channel.PreviewNone {
		return []Action{{Kind: KindUpdate, Event: ev}}, st
	}
	text := strings.TrimSpace(channel.RenderToolCallMessage(channel.BuildToolCallStart(tc)))
	var actions []Action
	splits := st.streamed
	if splits {
		flush := channel.StreamEvent{Type: channel.StreamEventFinal, Final: &channel.StreamFinalizePayload{}}
		actions = append(actions, Action{Kind: KindEndPreview, Final: &flush})
	}
	prompt := st.outbound(channel.Message{Format: channel.MessageFormatPlain, Text: text})
	actions = append(actions, Action{Kind: KindSend, Message: prompt, Event: ev})
	if splits {
		actions = append(actions, Action{Kind: KindOpenPreview})
		st.streamed, st.segment = false, ""
	}
	return actions, st
}

// planAttach sends the attachments of an event. A channel without a preview
// holds the reply text until the run ends, so the text that came before the
// attachments is sent first and the order of the events is kept.
func planAttach(ev channel.StreamEvent, st State) ([]Action, State) {
	var actions []Action
	if text := strings.TrimSpace(st.pending); text != "" {
		actions = append(actions, Action{Kind: KindSend, Message: st.outbound(channel.Message{Text: text})})
		st.delivered += st.pending
	}
	st.pending = ""
	actions = append(actions, Action{Kind: KindAttach, Event: ev, Attachments: ev.Attachments, Reply: st.replyRef(nil)})
	return actions, st
}

// planToolResult sends the state of a finished tool call as a message of its
// own, for a channel that has no preview to show it in.
func planToolResult(ev channel.StreamEvent, st State) ([]Action, State) {
	text := strings.TrimSpace(channel.RenderToolCallMessage(channel.BuildToolCallEnd(ev.ToolCall)))
	if text == "" {
		return observe(ev), st
	}
	msg := st.outbound(channel.Message{Format: channel.MessageFormatPlain, Text: text})
	return []Action{{Kind: KindSend, Message: msg, Event: ev}}, st
}

func needsAnswer(tc *channel.StreamToolCall) bool {
	return tc.ApprovalID != "" || channel.HasUserInputAction(tc.Actions)
}

// visibleText reports whether a delta puts text in front of the user.
func visibleText(ev channel.StreamEvent) bool {
	return ev.Delta != "" && ev.Phase != channel.StreamPhaseReasoning
}

func observe(ev channel.StreamEvent) []Action {
	return []Action{{Kind: KindObserve, Event: ev}}
}

func finalEvent(msg channel.OutboundMessage) channel.StreamEvent {
	return channel.StreamEvent{Type: channel.StreamEventFinal, Final: &channel.StreamFinalizePayload{Message: msg.Message}}
}

// reactions addresses each reaction to the source message.
func (st State) reactions(items []channel.ReactRequest) []channel.ReactRequest {
	out := make([]channel.ReactRequest, 0, len(items))
	for _, item := range items {
		out = append(out, channel.ReactRequest{Target: st.Target, MessageID: st.SourceMessageID, Emoji: item.Emoji})
	}
	return out
}

// outbound addresses msg to the reply target, quoting what replyRef returns.
func (st State) outbound(msg channel.Message) channel.OutboundMessage {
	msg.Reply = st.replyRef(msg.Reply)
	return channel.OutboundMessage{Target: st.Target, Message: msg}
}

// replyRef is the reference a message to the reply target quotes: its own, or
// the one the reply answers when it names none. Once the window has closed and
// the platform accepts a message without a reference, it is nil. The preview,
// the messages and the attachments of a reply all follow this rule.
func (st State) replyRef(own *channel.ReplyRef) *channel.ReplyRef {
	switch {
	case st.proactive:
		return nil
	case own == nil:
		return st.Reply
	}
	return own
}

// sendError delivers an error. Before any text has shown, the error goes
// through the preview, which holds nothing it could overwrite. After that, and
// on a channel without a preview, the error is a message of its own.
func sendError(ev channel.StreamEvent, caps channel.ChannelCapabilities, st State) ([]Action, State) {
	// Every error text, whether the preview shows it or a message carries it,
	// leaves through here, so this is the one place secrets are masked.
	ev.Error = redact.Text(ev.Error)
	if strings.TrimSpace(ev.Error) == "" {
		return nil, st
	}
	if !st.streamed && caps.Preview != channel.PreviewNone {
		return []Action{{Kind: KindUpdate, Event: ev}}, st
	}
	reach, next := windowOK(caps, st)
	if !reach {
		return []Action{{Kind: KindFail, Code: apperror.CodeChannelReplyWindowExpired}}, failed(next)
	}
	text := channel.ErrorReplyText(ev.ErrorCode, ev.Error)
	return []Action{{Kind: KindSend, Message: next.outbound(channel.Message{Text: text}), Event: ev}}, next
}

// windowOK reports whether a reply can still be sent, and updates State when
// the window has closed but the platform accepts a proactive message.
func windowOK(caps channel.ChannelCapabilities, st State) (bool, State) {
	if caps.ReplyWindow <= 0 || st.Now.Sub(st.ReceivedAt) <= caps.ReplyWindow {
		return true, st
	}
	if caps.ProactiveSend {
		st.proactive = true
		return true, st
	}
	return false, st
}

func failed(st State) State {
	st.failed = true
	return st
}
