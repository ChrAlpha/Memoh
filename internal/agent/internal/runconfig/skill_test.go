package runconfig

import (
	"testing"

	"github.com/felinics/memoh/internal/agent/agentapi/ports"
)

func TestNormalizeGatewaySkillPreservesPath(t *testing.T) {
	got, ok := normalizeGatewaySkill(ports.SkillEntry{
		Name:        "pdf",
		Description: "Read PDF instructions",
		Content:     "Use a PDF-aware workflow.",
		Path:        " /data/.agents/skills/pdf ",
	})
	if !ok {
		t.Fatal("normalizeGatewaySkill returned ok=false")
	}
	if got.Path != "/data/.agents/skills/pdf" {
		t.Fatalf("path = %q, want %q", got.Path, "/data/.agents/skills/pdf")
	}
}
