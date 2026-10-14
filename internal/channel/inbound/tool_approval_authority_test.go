package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/command"
)

// verdictApprovalGateway stands in for CanDecideToolApproval at the service:
// only the run's initiator may decide.
type verdictApprovalGateway struct {
	fakeChatGateway
	initiator string
	got       turn.ToolApprovalResponse
}

func (g *verdictApprovalGateway) RespondToolApproval(_ context.Context, input turn.ToolApprovalResponse, eventCh chan<- json.RawMessage) error {
	g.got = input
	if input.ActorUserID != g.initiator && input.ActorChannelIdentityID != g.initiator {
		return toolapproval.ErrForbidden
	}
	eventCh <- json.RawMessage(`{"type":"agent_end"}`)
	return nil
}

// The IM entry adds no check of its own: it hands the service the decider's
// identity and surfaces the service's verdict, the same as Web and HTTP.
func TestToolApprovalIMEntryAnswersWithTheServiceVerdict(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		identity InboundIdentity
		allowed  bool
	}{
		{"initiator by channel identity", InboundIdentity{BotID: "bot-1", ChannelIdentityID: "initiator"}, true},
		{"group member with a grant", InboundIdentity{BotID: "bot-1", ChannelIdentityID: "member", UserID: "member-account"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gateway := &verdictApprovalGateway{initiator: "initiator"}
			processor := NewChannelInboundProcessor(slog.New(slog.DiscardHandler), nil, nil, nil, gateway, nil, nil, "", 0)
			sender := &fakeReplySender{}
			msg := channel.InboundMessage{Channel: channel.ChannelType("telegram"), ReplyTarget: "group-chat", Message: channel.Message{ID: "approve-1"}}
			err := processor.handleToolApprovalCommand(context.Background(), msg, sender, tc.identity, "route-1", "session-1",
				command.Invocation{Parsed: command.ParsedCommand{Resource: "approve", Action: "approval-1"}})
			if gateway.got.ActorChannelIdentityID != tc.identity.ChannelIdentityID || gateway.got.ActorUserID != tc.identity.UserID ||
				gateway.got.ExplicitID != "approval-1" || gateway.got.Decision != "approve" {
				t.Fatalf("forwarded decision = %+v", gateway.got)
			}
			if tc.allowed {
				if err != nil {
					t.Fatalf("allowed decision error = %v", err)
				}
				return
			}
			if !errors.Is(err, toolapproval.ErrForbidden) {
				t.Fatalf("refused decision error = %v, want forbidden", err)
			}
			replied := false
			for _, event := range sender.events {
				replied = replied || event.Type == channel.StreamEventError
			}
			if !replied {
				t.Fatalf("refused decision told the member nothing: %+v", sender.events)
			}
		})
	}
}
