package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/channel"
)

func TestContinuationForwardsReplyAndInteractiveFollowUp(t *testing.T) {
	processor := NewChannelInboundProcessor(slog.Default(), nil, nil, nil, nil, nil, nil, "", 0)
	sender := &fakeReplySender{}
	msg := channel.InboundMessage{Channel: channel.ChannelType("telegram"), ReplyTarget: "test-chat", Message: channel.Message{ID: "answer"}}
	err := processor.streamContinuationCommand(context.Background(), msg, sender, InboundIdentity{BotID: "bot"}, func(_ context.Context, ch chan<- json.RawMessage) error {
		ch <- json.RawMessage(`{"type":"text_delta","delta":"收到你的答案"}`)
		ch <- json.RawMessage(`{"type":"user_input_request","toolName":"ask_user","toolCallId":"second-call","userInputId":"second-question","status":"pending","metadata":{"ui_payload":{"version":2,"questions":[{"id":"q1","kind":"text","text":"接下来做什么？"}]}}}`)
		ch <- json.RawMessage(`{"type":"agent_end","userInputId":"second-question","status":"pending"}`)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var text, question bool
	for _, event := range sender.events {
		if event.Type == channel.StreamEventDelta {
			text = true
		}
		if isUserInputEvent(&event) && event.ToolCall.CallID == "second-call" {
			question = true
		}
	}
	if !text || !question {
		t.Fatalf("reply=%v follow-up=%v events=%+v", text, question, sender.events)
	}
}

// A follow-up question that the channel cannot render natively reaches the
// user as a message of its own, the same as in a reply to a message.
func TestContinuationSendsPlainPromptWhenChannelHasNoNativeQuestion(t *testing.T) {
	processor := NewChannelInboundProcessor(slog.Default(), nil, nil, nil, nil, nil, nil, "", 0)
	sender := &fakeReplySender{plainPrompts: true}
	msg := channel.InboundMessage{Channel: channel.ChannelType("telegram"), ReplyTarget: "test-chat", Message: channel.Message{ID: "answer"}}
	err := processor.streamContinuationCommand(context.Background(), msg, sender, InboundIdentity{BotID: "bot"}, func(_ context.Context, ch chan<- json.RawMessage) error {
		ch <- json.RawMessage(`{"type":"user_input_request","toolName":"ask_user","toolCallId":"second-call","userInputId":"second-question","status":"pending","metadata":{"ui_payload":{"version":2,"questions":[{"id":"q1","kind":"text","text":"接下来做什么？"}]}}}`)
		ch <- json.RawMessage(`{"type":"agent_end","userInputId":"second-question","status":"pending"}`)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sender.sent) != 1 || !strings.Contains(sender.sent[0].Message.Text, "接下来做什么？") {
		t.Fatalf("sent = %+v, want the question as one plain message", sender.sent)
	}
	for _, event := range sender.events {
		if isUserInputEvent(&event) {
			t.Fatalf("the question reached the stream as an event: %+v", event)
		}
	}
}

// A continuation that hides tool calls still hands the hidden ordinary
// tool_call_start to the mapping layer, so the text streamed before it ends
// there rather than being overwritten by the reply. A channel that shows the
// tool calls keeps dropping the start, so the continuation stream never
// flashes a tool message a completed tool state is about to replace.
func TestContinuationHiddenToolCallEndsIMTextSegment(t *testing.T) {
	for _, tc := range []struct {
		channel channel.ChannelType
		want    []string
	}{
		{channel: channel.ChannelType("telegram"), want: []string{"Here is the answer."}},
		{channel: channel.ChannelType("web"), want: []string{"Let me check.", "Here is the answer."}},
	} {
		processor := NewChannelInboundProcessor(slog.Default(), nil, nil, nil, nil, nil, nil, "", 0)
		sender := &fakeReplySender{}
		msg := channel.InboundMessage{Channel: tc.channel, ReplyTarget: "test-chat", Message: channel.Message{ID: "approve"}}
		err := processor.streamContinuationCommand(context.Background(), msg, sender, InboundIdentity{BotID: "bot"}, func(_ context.Context, ch chan<- json.RawMessage) error {
			ch <- json.RawMessage(`{"type":"text_delta","delta":"Let me check."}`)
			ch <- json.RawMessage(`{"type":"tool_call_start","toolName":"read","toolCallId":"call-1"}`)
			ch <- json.RawMessage(`{"type":"text_delta","delta":"Here is the answer."}`)
			ch <- json.RawMessage(agentEnd(assistantText("Let me check."), assistantText("Here is the answer.")))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range sender.events {
			if event.Type == channel.StreamEventToolCallStart {
				t.Fatalf("%s: an ordinary tool call start reached the stream: %+v", tc.channel, event)
			}
		}
		var sent []string
		for _, out := range sender.sent {
			if out.Message.IsEmpty() {
				continue
			}
			sent = append(sent, out.Message.PlainText())
		}
		if !reflect.DeepEqual(sent, tc.want) {
			t.Fatalf("%s: delivered %v, want %v", tc.channel, sent, tc.want)
		}
	}
}

func TestAcceptanceReceiptPrecedesFailedContinuation(t *testing.T) {
	p := NewChannelInboundProcessor(slog.Default(), nil, nil, nil, nil, nil, nil, "", 0)
	sink := &fakeReplySender{}
	accepted := false
	sender := decisionReplySender{StreamReplySender: sink, accepted: func(context.Context, string) { accepted = true }}
	msg := channel.InboundMessage{Channel: channel.ChannelType("telegram"), ReplyTarget: "chat"}
	err := p.streamContinuationCommand(context.Background(), msg, sender, InboundIdentity{BotID: "bot"}, func(_ context.Context, ch chan<- json.RawMessage) error {
		ch <- json.RawMessage(`{"type":"decision_accepted","decision_id":"question"}`)
		return errors.New("SECRET transport failure after acceptance")
	})
	if err != nil || !accepted {
		t.Fatalf("accepted=%v err=%v", accepted, err)
	}
	found := false
	for _, event := range sink.events {
		if event.Type == channel.StreamEventError {
			found = true
			if strings.Contains(event.Error, "SECRET") {
				t.Fatal("private cause leaked")
			}
			if event.ErrorCode != "agent.response_interrupted" || event.Error != "The model response was interrupted. Please try again." {
				t.Fatalf("failure feedback = %+v, want the response_interrupted copy", event)
			}
		}
	}
	if !found {
		t.Fatal("missing output failure feedback")
	}
}
