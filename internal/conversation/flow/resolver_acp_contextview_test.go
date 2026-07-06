package flow

import (
	"context"
	"strings"
	"testing"

	"github.com/memohai/memoh/internal/contextview"
)

func TestACPContextViaContextViewAssemblesSections(t *testing.T) {
	t.Parallel()

	sections := []contextview.ACPSection{
		{ID: "acp.preamble", Text: "# Memoh ACP Context\n\npreamble body"},
		{ID: "acp.section.current-runtime", Text: "## Current Runtime\n\n- Bot ID: bot-1"},
	}
	markdown, uri := acpContextViaContextView(context.Background(), nil, sections, "")

	const want = "# Memoh ACP Context\n\npreamble body\n\n## Current Runtime\n\n- Bot ID: bot-1\n\n"
	if markdown != want {
		t.Fatalf("markdown = %q, want structural assembly %q", markdown, want)
	}
	if uri != acpContextURI {
		t.Fatalf("uri = %q, want %q", uri, acpContextURI)
	}
}

func TestACPContextSectionsSafeWithHeadingInsideFileExcerpt(t *testing.T) {
	t.Parallel()

	fileBlock := "## Long-Term Memory\n\nEmbedded excerpt from `/data/MEMORY.md`.\n\n```markdown\n# User Memory\nline before heading\n## Preferences\nprefers small patches\n```"
	sections := []contextview.ACPSection{
		{ID: "acp.preamble", Text: "# Memoh ACP Context\n\npreamble"},
		{ID: "acp.section.file.000", Text: fileBlock},
	}
	markdown, _ := acpContextViaContextView(context.Background(), nil, sections, "")

	if !strings.Contains(markdown, "line before heading\n## Preferences\nprefers small patches") {
		t.Fatalf("fence content must survive byte-for-byte:\n%s", markdown)
	}
}
