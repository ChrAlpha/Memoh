package native

import (
	"testing"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/agent/sessionmode"
)

const staticPromptTokenHeadroom = 8

var staticPromptTokenBaselines = map[string]int{
	sessionmode.Chat:      1227,
	sessionmode.Discuss:   1299,
	sessionmode.Heartbeat: 1307,
	sessionmode.Schedule:  1229,
	sessionmode.Subagent:  610,
}

func TestStaticPromptSizeBaselines(t *testing.T) {
	t.Parallel()

	for _, mode := range allPromptSessionTypes() {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			prompt := GenerateSystemPrompt(SystemPromptParams{
				SessionType: mode,
				Timezone:    "UTC",
			})
			got := contextfrag.TokensFromBytes(len(prompt))
			baseline := staticPromptTokenBaselines[mode]
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

			prompt := GenerateSystemPrompt(params)
			got := contextfrag.TokensFromBytes(len(prompt))
			if want := len(prompt) / contextfrag.EstimateBytesPerToken; got != want {
				t.Fatalf("byte estimator tokens = %d, want %d", got, want)
			}
		})
	}
}
