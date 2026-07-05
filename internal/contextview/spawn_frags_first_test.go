package contextview

import (
	"context"
	"reflect"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/agent/sessionmode"
	"github.com/memohai/memoh/internal/contextfrag"
)

// TestSpawnRunConfigFragsFirstMatchesLegacyReverseParse is the byte-equivalence
// gate for the subagent path: agentpkg.SpawnContextSourceFrags builds
// ContextSourceFrags directly from typed system sections
// (agentpkg.SystemSectionFrags over agentpkg.GenerateSystemSections) plus
// contextfrag.Compile, instead of letting cfg.ContextSourceFrags stay empty
// and having ApplyProviderRunConfig's legacy branch reverse-parse cfg.System.
// internal/agent cannot import internal/contextview (the reverse import
// already exists, for agentpkg.RunConfig), so this comparison lives here
// instead, mirroring resolver.go's TestPrepareRunConfigFragsFirstMatchesLegacyReverseParse.
// Both branches must still render to the identical provider System string and
// message stream, including through a dangling tool-call closure repair.
func TestSpawnRunConfigFragsFirstMatchesLegacyReverseParse(t *testing.T) {
	t.Parallel()

	dangling := sdk.Message{
		Role:    sdk.MessageRoleAssistant,
		Content: []sdk.MessagePart{sdk.ToolCallPart{ToolCallID: "call-lost", ToolName: "web_search", Input: map[string]any{}}},
	}
	materializedQuery := sdk.UserMessage("Current time: 2026-01-01T00:00:00Z\nwhat should we do next?")

	base := agentpkg.RunConfig{
		System: agentpkg.SpawnSystemPrompt(sessionmode.Subagent),
		Messages: []sdk.Message{
			sdk.UserMessage("earlier question"),
			dangling,
			sdk.UserMessage("next question"),
			materializedQuery,
		},
		Query:                    "what should we do next?",
		ContextQueryMaterialized: true,
		SessionType:              sessionmode.Subagent,
		ContextScope:             contextfrag.Scope{BotID: "bot-1", SessionID: "session-1"},
	}

	got1 := base
	got1.ContextSourceFrags = agentpkg.SpawnContextSourceFrags(got1)
	rendered1 := ApplyProviderRunConfig(context.Background(), nil, got1)

	got2 := base // ContextSourceFrags stays empty: ApplyProviderRunConfig takes the legacy collector branch.
	rendered2 := ApplyProviderRunConfig(context.Background(), nil, got2)

	if rendered1.System != rendered2.System {
		t.Fatalf("frags-first System diverges from legacy reverse-parse System:\ngot:  %q\nwant: %q", rendered1.System, rendered2.System)
	}
	if !reflect.DeepEqual(rendered1.Messages, rendered2.Messages) {
		t.Fatalf("messages diverge:\ngot:  %#v\nwant: %#v", rendered1.Messages, rendered2.Messages)
	}
}
