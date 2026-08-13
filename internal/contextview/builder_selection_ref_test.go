package contextview

import (
	"context"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
)

func TestSelectionDecisionsUsesContextRefWhenIDsCollide(t *testing.T) {
	first := contextfrag.TextFrag(contextfrag.TextFragInput{
		ID: "same-id", Kind: contextfrag.KindSystemPrompt, Role: sdk.MessageRoleSystem,
		Slot: contextfrag.SlotSystem, Text: "winner", Priority: 10,
		CacheClass: contextfrag.CacheStable, Trust: contextfrag.TrustSystem,
		Source: "selection-test", Collector: "selection-test", ConflictKey: "same-conflict",
	})
	second := contextfrag.TextFrag(contextfrag.TextFragInput{
		ID: "same-id", Kind: contextfrag.KindSystemPrompt, Role: sdk.MessageRoleSystem,
		Slot: contextfrag.SlotSystem, Text: "loser", Priority: 10,
		CacheClass: contextfrag.CacheStable, Trust: contextfrag.TrustWorkspace,
		Source: "selection-test", Collector: "selection-test", ConflictKey: "same-conflict",
	})
	sources := contextfrag.NormalizeContextRefs([]contextfrag.ContextFrag{first, second})
	builder := NewBuilder(
		NewMapCollectorRegistry(StaticCollector{CollectorName: "selection-test", Frags: sources}),
		&FragmentSelector{},
		IdentityPlacer{},
		NewMapRendererRegistry(),
	)
	view, err := builder.Build(context.Background(), BuildInput{
		Intent:  contextfrag.IntentRunConfigPreProvider,
		Sources: []SourceSpec{{Name: "selection-test"}},
		Options: BuildOptions{DryRun: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Selected) != 1 || view.Selected[0].Ref.ContentHash != sources[0].Ref.ContentHash {
		t.Fatalf("selector winner = %#v, want first ContextRef", view.Selected)
	}
	if len(view.Manifest.SelectionDecisions) != 2 {
		t.Fatalf("selection decisions = %#v, want two source decisions", view.Manifest.SelectionDecisions)
	}
	firstDecision := view.Manifest.SelectionDecisions[0]
	secondDecision := view.Manifest.SelectionDecisions[1]
	if firstDecision.Decision != contextfrag.DecisionSelected || firstDecision.Ref.ContentHash != sources[0].Ref.ContentHash {
		t.Fatalf("winner decision = %#v, want selected first ContextRef", firstDecision)
	}
	if secondDecision.Decision != contextfrag.DecisionDropped || secondDecision.Ref.ContentHash != sources[1].Ref.ContentHash {
		t.Fatalf("loser decision = %#v, want dropped second ContextRef", secondDecision)
	}
}
