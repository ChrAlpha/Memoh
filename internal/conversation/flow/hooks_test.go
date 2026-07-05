package flow

import (
	"strings"
	"testing"

	"github.com/memohai/memoh/internal/hooks"
)

func TestAfterPromptHookSystemBytesIncludesBeforeHookText(t *testing.T) {
	t.Parallel()
	hookTexts := []string{formatResolverHookContext(hooks.EventBeforePromptBuild, "extra guidance")}
	system := "base system prompt"

	got := afterPromptHookSystemBytes(system, hookTexts)

	want := len(system) + len("\n\n") + len(strings.Join(hookTexts, "\n\n"))
	if got != want {
		t.Fatalf("afterPromptHookSystemBytes = %d, want %d (must count the already-formatted before-hook text)", got, want)
	}
	if got <= len(system) {
		t.Fatalf("afterPromptHookSystemBytes = %d, want more than the bare system length %d when a before-hook injection exists", got, len(system))
	}
}

func TestAfterPromptHookSystemBytesNoHookText(t *testing.T) {
	t.Parallel()
	system := "base system prompt"
	if got := afterPromptHookSystemBytes(system, nil); got != len(system) {
		t.Fatalf("afterPromptHookSystemBytes = %d, want bare system length %d when there is no hook text", got, len(system))
	}
}
