package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/turn"
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

// What a turn with text before a tool call and two outputs after it (reasoning
// between them) shows: the text before the call ends as a segment of its own,
// the call follows it when the channel shows tool calls, and the final is the
// one message the two later outputs make up, without the text before the call.
var turnWithToolCall = []string{
	textDelta("Let me check."), toolStart, toolEnd, reasoningDelta("hm"), textDelta("Part one."), textDelta("Part two."),
}

func turnWithToolCallEnd() string {
	return agentEnd(assistantText("Let me check."), sendToolCallOther(),
		turn.ModelMessage{Role: "tool", Content: turn.NewTextContent("ok")},
		assistantText("Part one."), assistantText("Part two."))
}

func TestContinuationToolCallEndsIMTextSegment(t *testing.T) {
	for _, tc := range []struct {
		channel channel.ChannelType
		want    []string
	}{
		{channel: channel.ChannelType("telegram"), want: []string{
			"delta:Let me check.", "final:", "delta:hm", "delta:Part one.", "delta:Part two.", "final:Part one.\nPart two.",
		}},
		{channel: channel.ChannelType("web"), want: []string{
			"delta:Let me check.", "final:", "tool_call_start", "tool_call_end", "delta:hm", "delta:Part one.", "delta:Part two.", "final:Part one.\nPart two.",
		}},
	} {
		processor := NewChannelInboundProcessor(slog.Default(), nil, nil, nil, nil, nil, nil, "", 0)
		sender := &fakeReplySender{}
		msg := channel.InboundMessage{Channel: tc.channel, ReplyTarget: "test-chat", Message: channel.Message{ID: "approve"}}
		err := processor.streamContinuationCommand(context.Background(), msg, sender, InboundIdentity{BotID: "bot"}, func(_ context.Context, ch chan<- json.RawMessage) error {
			for _, payload := range append(append([]string{}, turnWithToolCall...), turnWithToolCallEnd()) {
				ch <- json.RawMessage(payload)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, event := range sender.events {
			switch event.Type {
			case channel.StreamEventDelta:
				got = append(got, "delta:"+event.Delta)
			case channel.StreamEventFinal:
				text := ""
				if event.Final != nil {
					text = event.Final.Message.Text
				}
				got = append(got, "final:"+text)
			case channel.StreamEventToolCallStart, channel.StreamEventToolCallEnd:
				got = append(got, string(event.Type))
			}
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: stream = %v, want %v", tc.channel, got, tc.want)
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
