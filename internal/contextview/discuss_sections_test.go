package contextview

import (
	"context"
	"reflect"
	"strings"
	"testing"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
	"github.com/memohai/memoh/internal/agent/sessionmode"
	"github.com/memohai/memoh/internal/chat/timeline"
)

func discussSectionsInputFixture() DiscussContextInput {
	return DiscussContextInput{
		ComposedMessages: []timeline.ContextMessage{{Role: "user", Content: "latest user"}},
	}
}

func TestCollectDiscussSourceFragsUsesProvidedSystemFrags(t *testing.T) {
	t.Parallel()

	scope := contextfrag.Scope{BotID: "bot-1", SessionID: "s1"}
	params := agentpkg.SystemPromptParams{
		SessionType: sessionmode.Discuss,
		Bot:         agentpkg.BotInfo{ID: "bot-1", Name: "research-bot"},
	}
	input := discussSectionsInputFixture()
	input.SystemFrags = agentpkg.SystemSectionFrags(agentpkg.GenerateSystemSections(params), scope)

	frags, err := (&DiscussSDKContextBuilder{}).CollectDiscussSourceFrags(
		context.Background(), scope, "REVERSE_PARSE_MUST_NOT_RUN", input)
	if err != nil {
		t.Fatalf("CollectDiscussSourceFrags error: %v", err)
	}
	if len(frags) <= len(input.SystemFrags) {
		t.Fatalf("frags = %d, want provided system frags followed by discuss frags", len(frags))
	}
	if !reflect.DeepEqual(frags[:len(input.SystemFrags)], input.SystemFrags) {
		t.Fatalf("leading frags = %#v, want the provided system frags verbatim", frags[:len(input.SystemFrags)])
	}
	kinds := make(map[contextfrag.Kind]bool, len(frags))
	for _, frag := range frags {
		kinds[frag.Kind] = true
		for _, part := range frag.Parts {
			if strings.Contains(part.Text, "REVERSE_PARSE_MUST_NOT_RUN") {
				t.Fatalf("flat system string must not be reverse-parsed when SystemFrags are provided: %#v", frag)
			}
		}
	}
	if !kinds[contextfrag.KindBotIdentity] {
		t.Fatalf("system frags must keep the typed bot identity kind, kinds = %#v", kinds)
	}
}

func TestCollectDiscussSourceFragsDoesNotAliasProvidedSystemFrags(t *testing.T) {
	t.Parallel()

	scope := contextfrag.Scope{BotID: "bot-1", SessionID: "s1"}
	params := agentpkg.SystemPromptParams{
		SessionType: sessionmode.Discuss,
		Bot:         agentpkg.BotInfo{ID: "bot-1", Name: "research-bot"},
	}
	provided := agentpkg.SystemSectionFrags(agentpkg.GenerateSystemSections(params), scope)
	withSpareCap := make([]contextfrag.ContextFrag, len(provided), len(provided)+8)
	copy(withSpareCap, provided)
	input := discussSectionsInputFixture()
	input.SystemFrags = withSpareCap

	frags, err := (&DiscussSDKContextBuilder{}).CollectDiscussSourceFrags(
		context.Background(), scope, "", input)
	if err != nil {
		t.Fatalf("CollectDiscussSourceFrags error: %v", err)
	}
	if len(frags) <= len(withSpareCap) {
		t.Fatalf("frags = %d, want discuss frags after the system frags", len(frags))
	}
	firstDiscussID := frags[len(withSpareCap)].ID

	_ = append(withSpareCap, contextfrag.ContextFrag{ID: "caller-sentinel"})

	if got := frags[len(withSpareCap)].ID; got != firstDiscussID {
		t.Fatalf("returned frags share the caller's backing array: frag ID = %q, want %q", got, firstDiscussID)
	}
}
