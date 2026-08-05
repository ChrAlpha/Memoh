package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
)

func TestDeterministicScenarioRunsAreByteIdentical(t *testing.T) {
	t.Parallel()

	first := t.TempDir()
	second := t.TempDir()
	if err := runDeterministicScenarios(first); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := runDeterministicScenarios(second); err != nil {
		t.Fatalf("second run: %v", err)
	}
	for _, name := range []string{s1OutputName, s2OutputName} {
		left, err := os.ReadFile(filepath.Join(first, name)) //nolint:gosec // fixed name under t.TempDir
		if err != nil {
			t.Fatalf("read first %s: %v", name, err)
		}
		right, err := os.ReadFile(filepath.Join(second, name)) //nolint:gosec // fixed name under t.TempDir
		if err != nil {
			t.Fatalf("read second %s: %v", name, err)
		}
		if !bytes.Equal(left, right) {
			t.Fatalf("%s differs across full scenario runs", name)
		}
	}
}

func TestS1FixtureMeetsDistributionContract(t *testing.T) {
	t.Parallel()

	fixture := buildS1Fixture()
	kindCounts := make(map[string]int)
	multilingualSkills := 0
	for _, frag := range fixture.systemFrags {
		kindCounts[string(frag.Kind)]++
		if frag.Kind == "skills_catalog" && stringsInFrag(frag, "🙂") {
			multilingualSkills++
		}
	}
	if kindCounts["skills_catalog"] != 41 || kindCounts["workspace_instruction"] != 6 || kindCounts["platform_identity"] != 9 || kindCounts["tool_usage"] != 11 {
		t.Fatalf("fixture system counts = %#v", kindCounts)
	}
	if multilingualSkills < 12 {
		t.Fatalf("multilingual skills = %d, want at least 12", multilingualSkills)
	}
	if len(fixture.messages) != 31 || len(fixture.tools) < 10 {
		t.Fatalf("messages/tools = %d/%d", len(fixture.messages), len(fixture.tools))
	}
}

func stringsInFrag(frag contextfrag.ContextFrag, value string) bool {
	for _, part := range frag.Parts {
		if strings.Contains(part.Text, value) {
			return true
		}
	}
	return false
}
