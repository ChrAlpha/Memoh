package transcript

import (
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runreq"
)

func TestDedupePersistedCurrentUserMessageRemovesCurrentInboundFromHistory(t *testing.T) {
	t.Parallel()

	history := []historyfrag.HistoryRecord{
		agenttest.HistoryRecord("row-1", runreq.ModelMessage{
			Role:    "user",
			Content: runreq.NewTextContent("---\nmessage-id: qq-msg-1\nchannel: qq\n---\nhello"),
		}, func(record *historyfrag.HistoryRecord) {
			record.ExternalMessageID = "qq-msg-1"
			record.Platform = "qq"
			record.SenderChannelIdentityID = "channel-identity-1"
		}),
		agenttest.HistoryRecord("row-2", runreq.ModelMessage{
			Role:    "assistant",
			Content: runreq.NewTextContent("ok"),
		}, nil),
	}

	got := dedupePersistedCurrentUserMessage(history, runreq.ChatRequest{
		UserMessagePersisted:    true,
		RouteID:                 "route-1",
		ExternalMessageID:       "qq-msg-1",
		CurrentChannel:          "qq",
		SourceChannelIdentityID: "channel-identity-1",
	})

	if len(got) != 1 {
		t.Fatalf("expected 1 message after dedupe, got %d", len(got))
	}
	if got[0].ModelMessage.Role != "assistant" {
		t.Fatalf("unexpected remaining role: %s", got[0].ModelMessage.Role)
	}
}
