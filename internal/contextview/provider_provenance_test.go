package contextview

import (
	"context"
	"reflect"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/agent/turn"
)

// The view hands the run what each message it renders stands for, by
// position: what it adds (the hook context before the input) stands for
// nothing, and a closure it repairs stands for the message that made the
// call, so a fold removes the two together.
func TestProviderViewCarriesProvenanceByPosition(t *testing.T) {
	t.Parallel()

	row := func(id string) []turn.ContextMessageSource {
		return []turn.ContextMessageSource{{Kind: "history", ID: id}}
	}
	call := sdk.Message{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{sdk.ToolCallPart{
		ToolCallID: "call-1", ToolName: "exec", Input: toolexec.ArgumentsFromValue(map[string]any{}),
	}}}
	current := 2
	cfg := native.RunConfig{
		Messages: []sdk.Message{sdk.UserMessage("earlier question"), call, sdk.UserMessage("current input")},
		ContextProvenance: []native.MessageProvenance{
			{Sources: row("question")},
			{Sources: row("call")},
			{Sources: []turn.ContextMessageSource{{Kind: "history", ID: "input"}}, Anchor: true},
		},
		ContextCurrentUserMessageIndex: &current,
		ContextQueryMaterialized:       true,
		ContextHookText:                "hook context",
		ContextTrimmableMessages:       2,
	}
	cfg.ContextSourceFrags = CollectNonSystemProviderSourceFrags(context.Background(), cfg)

	got := ApplyProviderRunConfig(context.Background(), nil, cfg)

	roles := make([]sdk.MessageRole, len(got.Messages))
	for i, message := range got.Messages {
		roles[i] = message.Role
	}
	wantRoles := []sdk.MessageRole{sdk.MessageRoleUser, sdk.MessageRoleAssistant, sdk.MessageRoleTool, sdk.MessageRoleUser, sdk.MessageRoleUser}
	if !reflect.DeepEqual(roles, wantRoles) {
		t.Fatalf("rendered roles = %v, want %v", roles, wantRoles)
	}
	want := []native.MessageProvenance{
		{Sources: row("question")},
		{Sources: row("call")},
		{Sources: row("call")},
		{},
		{Sources: []turn.ContextMessageSource{{Kind: "history", ID: "input"}}, Anchor: true},
	}
	if len(got.ContextProvenance) != len(got.Messages) {
		t.Fatalf("provenance for %d messages, request has %d", len(got.ContextProvenance), len(got.Messages))
	}
	for i, w := range want {
		p := got.ContextProvenance[i]
		if !reflect.DeepEqual(p.Sources, w.Sources) || p.Anchor != w.Anchor {
			t.Fatalf("message %d renders %v anchor=%v, want %v anchor=%v", i, p.Sources, p.Anchor, w.Sources, w.Anchor)
		}
	}
}
