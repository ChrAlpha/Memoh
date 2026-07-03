package contextview

import (
	"context"
	"testing"

	"github.com/memohai/memoh/internal/contextfrag"
)

func TestACPSectionsCollectorMapsSectionMetadata(t *testing.T) {
	t.Parallel()

	collector := &ACPSectionsCollector{}
	frags, err := collector.Collect(context.Background(), CollectRequest{
		Scope:  contextfrag.Scope{BotID: "bot-1"},
		Intent: contextfrag.IntentACPRuntimePrompt,
		Config: ACPSectionsConfig{Sections: []ACPSection{
			{
				ID:         "acp.preamble",
				Text:       "# Memoh ACP Context",
				Kind:       contextfrag.KindSystemPolicy,
				Priority:   10,
				CacheClass: contextfrag.CacheStable,
				Budget:     contextfrag.BudgetPolicy{Overflow: contextfrag.OverflowKeep},
			},
			{
				ID:    "acp.section.file.000",
				Text:  "## Bot Soul\n\nsoul text",
				Kind:  contextfrag.KindWorkspaceInstruction,
				Trust: contextfrag.TrustWorkspace,
			},
			{ID: "acp.section.runtime-notes", Text: "notes"},
		}},
	})
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(frags) != 3 {
		t.Fatalf("frags = %d, want 3", len(frags))
	}

	preamble := frags[0]
	if preamble.Kind != contextfrag.KindSystemPolicy || preamble.Priority != 10 ||
		preamble.CacheClass != contextfrag.CacheStable || preamble.Budget.Overflow != contextfrag.OverflowKeep {
		t.Fatalf("preamble metadata not mapped: %+v", preamble)
	}

	file := frags[1]
	if file.Kind != contextfrag.KindWorkspaceInstruction || file.Trust != contextfrag.TrustWorkspace {
		t.Fatalf("file section metadata not mapped: kind=%s trust=%s", file.Kind, file.Trust)
	}

	defaulted := frags[2]
	if defaulted.Kind != contextfrag.KindACPContext || defaulted.Trust != contextfrag.TrustSystem ||
		defaulted.Priority != 35 || defaulted.CacheClass != contextfrag.CacheDynamic {
		t.Fatalf("zero-value section must keep collector defaults: %+v", defaulted)
	}
}
