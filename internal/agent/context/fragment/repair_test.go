package contextfrag

import (
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/toolexec"
)

func TestRepairToolClosureFragsRefreshesRebuiltMessageAccounting(t *testing.T) {
	t.Parallel()

	assistant := MessageFrag(MessageFragInput{
		ID: "history.assistant",
		Message: sdk.Message{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{sdk.ToolCallPart{
			ToolCallID: "call-valid",
			ToolName:   "search",
		}}},
		Kind: KindConversationEvent,
		Slot: SlotHistory,
	})
	result := MessageFrag(MessageFragInput{
		ID: "history.result",
		Message: sdk.Message{Role: sdk.MessageRoleTool, Content: []sdk.MessagePart{
			sdk.ToolResultPart{ToolCallID: "call-orphan", ToolName: "search", Result: toolexec.OutputFromValue("orphan")},
			sdk.ToolResultPart{ToolCallID: "call-valid", ToolName: "search", Result: toolexec.OutputFromValue("kept")},
		}},
		Kind:          KindConversationEvent,
		Slot:          SlotHistory,
		TokenEstimate: 999,
	})
	result.ConflictKey = "history.tool-result"
	result = WithContextRef(result, ContextRef{
		Namespace:  "history",
		ID:         "result-row",
		Version:    3,
		Schema:     SchemaContextRef,
		Durability: RefDurable,
	})

	repaired := RepairToolClosureFrags([]ContextFrag{assistant, result}, Scope{}, "test")
	if len(repaired) != 2 {
		t.Fatalf("repaired fragments = %d, want 2", len(repaired))
	}
	got := repaired[1]
	msg := FragMessage(got)
	if msg == nil || len(msg.Content) != 1 {
		t.Fatalf("rebuilt tool result = %#v, want one valid result", msg)
	}
	kept, ok := msg.Content[0].(sdk.ToolResultPart)
	if !ok || kept.ToolCallID != "call-valid" {
		t.Fatalf("rebuilt tool result content = %#v, want call-valid", msg.Content[0])
	}
	if got.TokenEstimate != 0 {
		t.Fatalf("rebuilt token estimate = %d, want stale estimate cleared", got.TokenEstimate)
	}
	if resolved, estimated := ResolveFragTokens(got), EstimateFragTokens(got); resolved != estimated || resolved == 999 {
		t.Fatalf("rebuilt token accounting = %d, estimated = %d, stale = 999", resolved, estimated)
	}
	if got.Ref != result.Ref {
		t.Fatalf("rebuilt ref = %#v, want the identity it had %#v", got.Ref, result.Ref)
	}
	if got.ConflictKey != result.ConflictKey {
		t.Fatalf("rebuilt conflict key = %q, want %q", got.ConflictKey, result.ConflictKey)
	}
}
