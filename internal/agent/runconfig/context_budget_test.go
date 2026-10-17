package runconfig

import (
	"testing"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/transcript"
	"github.com/felinics/memoh/internal/models"
)

func TestEffectiveContextTokenBudget(t *testing.T) {
	window := func(n int) models.GetResponse {
		return models.GetResponse{Model: models.Model{Config: models.ModelConfig{ContextWindow: &n}}}
	}
	cases := []struct {
		name     string
		model    models.GetResponse
		fallback int
		want     int
	}{
		{"window below default fallback", window(128_000), 0, 128_000},
		{"window above default fallback", window(1_000_000), 0, 1_000_000},
		{"window ignores configured fallback", window(1_000_000), 50_000, 1_000_000},
		{"no window uses default fallback", models.GetResponse{}, 0, contextfrag.DefaultContextFallbackTokens},
		{"no window uses configured fallback", models.GetResponse{}, 64_000, 64_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newBuilder(func(d *Deps) {
				d.Transcript = transcript.New(transcript.Deps{FallbackTokens: tc.fallback})
			})
			if got := svc.effectiveContextTokenBudget(tc.model); got != tc.want {
				t.Fatalf("budget = %d, want %d", got, tc.want)
			}
		})
	}
}
