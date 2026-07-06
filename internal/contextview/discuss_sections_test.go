package contextview

import (
	"context"
	"reflect"
	"strings"
	"testing"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/agent/sessionmode"
	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/pipeline"
)

func discussSectionsInputFixture() pipeline.DiscussContextInput {
	return pipeline.DiscussContextInput{
		RC: pipeline.RenderedContext{
			renderedTextSegment(100, "first rc"),
			renderedTextSegment(300, "second rc"),
		},
		TRs: []pipeline.TurnResponseEntry{{
			RequestedAtMs: 200,
			Role:          "assistant",
			Content:       "assistant turn",
		}},
		CompactSummary: "older summary",
		LateBinding:    "Only answer if mentioned.",
	}
}

// TestCollectDiscussSourceFragsUsesProvidedSystemFrags proves the discuss
// builder consumes caller-provided typed system fragments instead of
// reverse-parsing the flat system string through SystemPromptCollector.
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

// TestCollectDiscussSourceFragsSectionsMatchReverseParseRender is the
// byte-equivalence gate for the discuss system prompt switch: for the same
// SystemPromptParams, the sections-first path (SystemFrags built from
// GenerateSystemSections) and the legacy reverse-parse path (flat
// GenerateSystemPrompt string through SystemPromptCollector) must produce
// identical final renders on both provider targets — the SDK messages view
// and the discuss ACP prompt.
func TestCollectDiscussSourceFragsSectionsMatchReverseParseRender(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		params agentpkg.SystemPromptParams
	}{
		{
			name:   "minimal",
			params: agentpkg.SystemPromptParams{SessionType: sessionmode.Discuss},
		},
		{
			name: "full",
			params: agentpkg.SystemPromptParams{
				SessionType:               sessionmode.Discuss,
				Bot:                       agentpkg.BotInfo{ID: "bot-1", Name: "research-bot", DisplayName: "Research Bot", Timezone: "Asia/Shanghai"},
				Skills:                    []agentpkg.SkillEntry{{Name: "foo-skill", Description: "does foo things"}},
				Files:                     []agentpkg.SystemFile{{Filename: "AGENTS.md", Content: "workspace rules"}},
				Timezone:                  "Asia/Shanghai",
				PlatformIdentitiesSection: "## Platform identities\n\n- telegram: `12345`",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scope := contextfrag.Scope{BotID: "bot-1", SessionID: "s1"}
			system := agentpkg.GenerateSystemPrompt(tc.params)
			builder := &DiscussSDKContextBuilder{}

			legacyInput := discussSectionsInputFixture()
			legacyFrags, err := builder.CollectDiscussSourceFrags(context.Background(), scope, system, legacyInput)
			if err != nil {
				t.Fatalf("legacy CollectDiscussSourceFrags error: %v", err)
			}
			sectionsInput := discussSectionsInputFixture()
			sectionsInput.SystemFrags = agentpkg.SystemSectionFrags(agentpkg.GenerateSystemSections(tc.params), scope)
			sectionsFrags, err := builder.CollectDiscussSourceFrags(context.Background(), scope, system, sectionsInput)
			if err != nil {
				t.Fatalf("sections CollectDiscussSourceFrags error: %v", err)
			}

			legacyRendered := ApplyProviderRunConfig(context.Background(), nil,
				agentpkg.RunConfig{ContextSourceFrags: legacyFrags, ContextScope: scope})
			sectionsRendered := ApplyProviderRunConfig(context.Background(), nil,
				agentpkg.RunConfig{ContextSourceFrags: sectionsFrags, ContextScope: scope})
			if sectionsRendered.System != legacyRendered.System {
				t.Fatalf("sections System diverges from reverse-parse System:\ngot:  %q\nwant: %q",
					sectionsRendered.System, legacyRendered.System)
			}
			if !reflect.DeepEqual(sectionsRendered.Messages, legacyRendered.Messages) {
				t.Fatalf("sections messages diverge:\ngot:  %#v\nwant: %#v",
					sectionsRendered.Messages, legacyRendered.Messages)
			}

			legacyPrompt, err := builder.BuildDiscussACPPrompt(context.Background(), scope, legacyInput)
			if err != nil {
				t.Fatalf("legacy BuildDiscussACPPrompt error: %v", err)
			}
			sectionsPrompt, err := builder.BuildDiscussACPPrompt(context.Background(), scope, sectionsInput)
			if err != nil {
				t.Fatalf("sections BuildDiscussACPPrompt error: %v", err)
			}
			if sectionsPrompt != legacyPrompt {
				t.Fatalf("sections ACP prompt diverges:\ngot:  %q\nwant: %q", sectionsPrompt, legacyPrompt)
			}
		})
	}
}
