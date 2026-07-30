package native

import (
	"testing"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/agent/sessionmode"
)

const (
	staticPromptTokenHeadroom           = 8
	staticPromptChatBaselineTokens      = 1227
	staticPromptDiscussBaselineTokens   = 1299
	staticPromptHeartbeatBaselineTokens = 1307
	staticPromptScheduleBaselineTokens  = 1229
	staticPromptSubagentBaselineTokens  = 610
)

var staticPromptTokenBaselines = map[string]int{
	sessionmode.Chat:      staticPromptChatBaselineTokens,
	sessionmode.Discuss:   staticPromptDiscussBaselineTokens,
	sessionmode.Heartbeat: staticPromptHeartbeatBaselineTokens,
	sessionmode.Schedule:  staticPromptScheduleBaselineTokens,
	sessionmode.Subagent:  staticPromptSubagentBaselineTokens,
}

func TestStaticPromptSizeBaselines(t *testing.T) {
	t.Parallel()

	for _, mode := range allPromptSessionTypes() {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			prompt := renderSystemSections(GenerateSystemSections(SystemPromptParams{
				SessionType: mode,
				Timezone:    "UTC",
			}))
			got := contextfrag.TokensFromBytes(len(prompt))
			baseline, ok := staticPromptTokenBaselines[mode]
			if !ok {
				t.Fatalf("missing static prompt baseline for mode %q", mode)
			}
			t.Logf("static prompt tokens = %d, bytes = %d", got, len(prompt))
			if got > baseline+staticPromptTokenHeadroom {
				t.Fatalf(
					"static prompt tokens = %d, baseline = %d, headroom = %d",
					got,
					baseline,
					staticPromptTokenHeadroom,
				)
			}
		})
	}
}

func TestGenerateSystemSectionsPolicyForRemainingNativeModes(t *testing.T) {
	t.Parallel()

	want := []goldenSectionExpectation{
		{"system.prompt.intro", contextfrag.KindSystemPrompt, 10, contextfrag.RetentionRequired},
		{"system.bot_identity", contextfrag.KindBotIdentity, 20, contextfrag.RetentionPreferred},
		{"system.prompt.body", contextfrag.KindSystemPrompt, 30, contextfrag.RetentionRequired},
		{"system.prompt.tail", contextfrag.KindSystemPrompt, 50, contextfrag.RetentionRequired},
		{"system.platform_identity.header", contextfrag.KindPlatformIdentity, 60, contextfrag.RetentionPreferred},
		{"system.platform_identity.telegram-1", contextfrag.KindPlatformIdentity, 60, contextfrag.RetentionPreferred},
		{"system.skills.header", contextfrag.KindSkillsCatalog, 65, contextfrag.RetentionOptional},
		{"system.skill.bar-skill", contextfrag.KindSkillsCatalog, 65, contextfrag.RetentionOptional},
		{"system.skill.foo-skill", contextfrag.KindSkillsCatalog, 65, contextfrag.RetentionOptional},
		{"system.workspace_file.AGENTS.md", contextfrag.KindWorkspaceInstruction, 70, contextfrag.RetentionPreferred},
		{"system.workspace_file.PROFILES.md", contextfrag.KindWorkspaceInstruction, 70, contextfrag.RetentionPreferred},
	}

	for _, mode := range []string{sessionmode.Discuss, sessionmode.Heartbeat, sessionmode.Schedule} {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			sections := GenerateSystemSections(SystemPromptParams{
				SessionType:               mode,
				Timezone:                  "UTC",
				Bot:                       goldenFullBot,
				Skills:                    goldenFullSkills,
				Files:                     goldenFullFiles,
				PlatformIdentitiesSection: goldenFullPlatform,
				PlatformIdentities:        goldenFullPlatformItems,
			})
			assertSectionTable(t, sections, want)
		})
	}
}

func TestStaticPromptBaselineUsesByteEstimatorWithoutTokenizer(t *testing.T) {
	t.Parallel()

	for _, mode := range allPromptSessionTypes() {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			params := SystemPromptParams{SessionType: mode, Timezone: "UTC"}
			for _, frag := range SystemSectionFrags(GenerateSystemSections(params), contextfrag.Scope{}) {
				if frag.TokenEstimate != 0 {
					t.Fatalf("static fragment %s has preset token estimate %d", frag.ID, frag.TokenEstimate)
				}
			}

			prompt := renderSystemSections(GenerateSystemSections(params))
			got := contextfrag.TokensFromBytes(len(prompt))
			if want := len(prompt) / contextfrag.EstimateBytesPerToken; got != want {
				t.Fatalf("byte estimator tokens = %d, want %d", got, want)
			}
		})
	}
}
