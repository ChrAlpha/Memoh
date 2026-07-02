package contextview

import (
	"context"
	"strings"
	"testing"

	"github.com/memohai/memoh/internal/contextfrag"
	sdk "github.com/memohai/twilight-ai/sdk"
)

func TestBuildEndToEnd_PassthroughProfile(t *testing.T) {
	frags := testFrags()
	builder := NewBuilder(
		NewMapCollectorRegistry(StaticCollector{CollectorName: "static", Frags: frags}),
		PassthroughSelector{},
		IdentityPlacer{},
		NewMapRendererRegistry(NoopRenderer{TargetName: contextfrag.RenderAuditManifest}),
	)

	got, err := builder.Build(context.Background(), BuildInput{
		Scope:   contextfrag.Scope{BotID: "bot-1", SessionID: "session-1"},
		Intent:  contextfrag.IntentRunConfigPreProvider,
		Sources: []SourceSpec{{Name: "static", Config: map[string]any{"kind": "fixture"}}},
		Targets: []contextfrag.RenderTarget{contextfrag.RenderAuditManifest},
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	if got == nil {
		t.Fatal("Build returned nil ContextView")
	}
	if len(got.SourceFrags) != 3 {
		t.Fatalf("source frags: got %d, want 3", len(got.SourceFrags))
	}
	if len(got.Selected) != 3 {
		t.Fatalf("selected frags: got %d, want 3", len(got.Selected))
	}
	if got.Manifest.Counts.Fragments != 3 {
		t.Fatalf("manifest fragments: got %d, want 3", got.Manifest.Counts.Fragments)
	}
	if got.Manifest.View != contextfrag.ViewRunConfigPreProvider {
		t.Fatalf("manifest view: got %q, want %q", got.Manifest.View, contextfrag.ViewRunConfigPreProvider)
	}
	if _, ok := got.Rendered[contextfrag.RenderAuditManifest]; !ok {
		t.Fatalf("rendered targets: got %#v, want audit_manifest", got.Rendered)
	}
	if _, ok := got.Trace.CollectDurations["static"]; !ok {
		t.Fatalf("missing collect duration for static: %#v", got.Trace.CollectDurations)
	}
	if got.Trace.SelectionSummary.TotalCollected != 3 {
		t.Fatalf("trace collected count: got %d, want 3", got.Trace.SelectionSummary.TotalCollected)
	}
	if got.Trace.SelectionSummary.TotalSelected != 3 {
		t.Fatalf("trace selected count: got %d, want 3", got.Trace.SelectionSummary.TotalSelected)
	}
	if got.Trace.SelectionSummary.TotalDropped != 0 {
		t.Fatalf("trace dropped count: got %d, want 0", got.Trace.SelectionSummary.TotalDropped)
	}
	summary, ok := got.Trace.RenderSummaries[contextfrag.RenderAuditManifest]
	if !ok {
		t.Fatalf("trace render summary: got %#v, want audit_manifest", got.Trace.RenderSummaries)
	}
	if summary.ItemCount != 3 {
		t.Fatalf("render item count: got %d, want 3", summary.ItemCount)
	}
}

func TestBuildDryRun_SkipsRender(t *testing.T) {
	builder := NewBuilder(
		NewMapCollectorRegistry(StaticCollector{CollectorName: "static", Frags: testFrags()[:1]}),
		PassthroughSelector{},
		IdentityPlacer{},
		NewMapRendererRegistry(),
	)

	got, err := builder.Build(context.Background(), BuildInput{
		Intent:  contextfrag.IntentRunConfigPreProvider,
		Sources: []SourceSpec{{Name: "static"}},
		Targets: []contextfrag.RenderTarget{contextfrag.RenderAuditManifest},
		Options: BuildOptions{DryRun: true},
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	if len(got.Rendered) != 0 {
		t.Fatalf("rendered output: got %#v, want none", got.Rendered)
	}
	if len(got.Trace.RenderSummaries) != 0 {
		t.Fatalf("render trace: got %#v, want none", got.Trace.RenderSummaries)
	}
}

func TestBuildUnknownCollector_ReturnsError(t *testing.T) {
	builder := NewBuilder(
		NewMapCollectorRegistry(),
		PassthroughSelector{},
		IdentityPlacer{},
		NewMapRendererRegistry(),
	)

	_, err := builder.Build(context.Background(), BuildInput{
		Intent:  contextfrag.IntentRunConfigPreProvider,
		Sources: []SourceSpec{{Name: "missing"}},
		Options: BuildOptions{DryRun: true},
	})
	if err == nil {
		t.Fatal("Build returned nil error")
	}
	if !strings.Contains(err.Error(), `unknown collector "missing"`) {
		t.Fatalf("error: got %q, want unknown collector name", err)
	}
}

func TestBuildUsesExactCollectorName(t *testing.T) {
	t.Parallel()

	builder := NewBuilder(
		NewMapCollectorRegistry(StaticCollector{CollectorName: " spaced ", Frags: testFrags()[:1]}),
		PassthroughSelector{},
		IdentityPlacer{},
		NewMapRendererRegistry(),
	)

	view, err := builder.Build(context.Background(), BuildInput{
		Intent:  contextfrag.IntentRunConfigPreProvider,
		Sources: []SourceSpec{{Name: " spaced "}},
		Options: BuildOptions{DryRun: true},
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	if _, ok := view.Trace.CollectDurations[" spaced "]; !ok {
		t.Fatalf("collect durations should use exact source name: %#v", view.Trace.CollectDurations)
	}
}

func TestBuildNormalizesCollectedContextRefs(t *testing.T) {
	t.Parallel()

	frag := contextfrag.TextFrag(contextfrag.TextFragInput{
		ID:        "system.prompt",
		Kind:      contextfrag.KindSystemPrompt,
		Role:      sdk.MessageRoleSystem,
		Slot:      contextfrag.SlotSystem,
		Text:      "system prompt",
		Source:    "static",
		Collector: "static",
	})
	builder := NewBuilder(
		NewMapCollectorRegistry(StaticCollector{CollectorName: "static", Frags: []contextfrag.ContextFrag{frag}}),
		PassthroughSelector{},
		IdentityPlacer{},
		NewMapRendererRegistry(),
	)

	view, err := builder.Build(context.Background(), BuildInput{
		Intent:  contextfrag.IntentRunConfigPreProvider,
		Sources: []SourceSpec{{Name: "static"}},
		Options: BuildOptions{DryRun: true},
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	if view.SourceFrags[0].Ref.ID == "" {
		t.Fatal("SourceFrags should contain normalized refs")
	}
	if view.Selected[0].Ref.ContentHash == "" {
		t.Fatal("Selected should contain normalized refs before selection output")
	}
	if view.Placement.Items[0].Ref.Schema != contextfrag.SchemaContextRef {
		t.Fatalf("Placement ref schema = %q, want %q", view.Placement.Items[0].Ref.Schema, contextfrag.SchemaContextRef)
	}
}

func TestBuildUnknownRenderer_ReturnsError(t *testing.T) {
	builder := NewBuilder(
		NewMapCollectorRegistry(StaticCollector{CollectorName: "static", Frags: testFrags()[:1]}),
		PassthroughSelector{},
		IdentityPlacer{},
		NewMapRendererRegistry(),
	)

	_, err := builder.Build(context.Background(), BuildInput{
		Intent:  contextfrag.IntentRunConfigPreProvider,
		Sources: []SourceSpec{{Name: "static"}},
		Targets: []contextfrag.RenderTarget{contextfrag.RenderAuditManifest},
	})
	if err == nil {
		t.Fatal("Build returned nil error")
	}
	if !strings.Contains(err.Error(), `unknown renderer "audit_manifest"`) {
		t.Fatalf("error: got %q, want unknown renderer target", err)
	}
}

func TestManifestTracksAllFragments(t *testing.T) {
	frags := testFrags()
	builder := NewBuilder(
		NewMapCollectorRegistry(StaticCollector{CollectorName: "static", Frags: frags}),
		PassthroughSelector{},
		IdentityPlacer{},
		NewMapRendererRegistry(),
	)

	got, err := builder.Build(context.Background(), BuildInput{
		Intent:  contextfrag.IntentDiscussReply,
		Sources: []SourceSpec{{Name: "static"}},
		Options: BuildOptions{DryRun: true},
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	if got.Manifest.View != contextfrag.ViewDiscussReply {
		t.Fatalf("manifest view: got %q, want %q", got.Manifest.View, contextfrag.ViewDiscussReply)
	}
	if len(got.Manifest.Items) != len(frags) {
		t.Fatalf("manifest items: got %d, want %d", len(got.Manifest.Items), len(frags))
	}
	wantIDs := map[string]bool{
		"system.prompt":   false,
		"history.001":     false,
		"current.message": false,
	}
	for _, item := range got.Manifest.Items {
		_, ok := wantIDs[item.ID]
		if !ok {
			t.Fatalf("unexpected manifest item id %q", item.ID)
		}
		wantIDs[item.ID] = true
	}
	for id, seen := range wantIDs {
		if !seen {
			t.Fatalf("manifest missing item id %q", id)
		}
	}
}

func TestManifestEditTraceRecordsDroppedFragments(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		messageFrag("old-user", sdk.UserMessage("old question")),
		messageFrag("old-assistant", sdk.AssistantMessage("old answer")),
		messageFrag("latest", sdk.UserMessage("latest question")),
	}
	builder := NewBuilder(
		NewMapCollectorRegistry(StaticCollector{CollectorName: "static", Frags: frags}),
		&FragmentSelector{},
		StablePrefixPlacer{},
		NewMapRendererRegistry(),
	)

	got, err := builder.Build(context.Background(), BuildInput{
		Intent:  contextfrag.IntentRunConfigPreProvider,
		Sources: []SourceSpec{{Name: "static"}},
		Budget:  BudgetEnvelope{MaxTokens: 1},
		Options: BuildOptions{DryRun: true},
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	if len(got.Manifest.Items) != 2 ||
		got.Manifest.Items[0].ID != "history.trim_notice" ||
		got.Manifest.Items[1].ID != "latest" {
		t.Fatalf("manifest selected items = %#v, want trim notice then latest", got.Manifest.Items)
	}
	if len(got.Manifest.EditTrace) != 2 {
		t.Fatalf("edit trace = %#v, want two dropped fragments", got.Manifest.EditTrace)
	}
	for _, edit := range got.Manifest.EditTrace {
		if edit.Op != contextfrag.EditRemove || edit.Slot != contextfrag.SlotHistory {
			t.Fatalf("edit trace entry = %#v, want history remove", edit)
		}
		if len(edit.Refs) != 1 || edit.Refs[0].ID == "" || edit.Refs[0].Schema != contextfrag.SchemaContextRef {
			t.Fatalf("edit trace refs = %#v, want normalized context ref", edit.Refs)
		}
	}
}

func TestAuditRenderUsesBuilderManifest(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		messageFrag("old-user", sdk.UserMessage("old question")),
		messageFrag("latest", sdk.UserMessage("latest question")),
	}
	builder := NewBuilder(
		NewMapCollectorRegistry(StaticCollector{CollectorName: "static", Frags: frags}),
		&FragmentSelector{},
		StablePrefixPlacer{},
		NewMapRendererRegistry(&AuditManifestRenderer{}),
	)

	got, err := builder.Build(context.Background(), BuildInput{
		Intent: contextfrag.IntentRunConfigPreProvider,
		Sources: []SourceSpec{{
			Name: "static",
		}},
		Targets: []contextfrag.RenderTarget{contextfrag.RenderAuditManifest},
		Budget:  BudgetEnvelope{MaxTokens: 1},
		DynamicMutators: []contextfrag.DynamicMutator{
			contextfrag.DynamicMutatorPromptCache,
		},
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	payload, ok := got.Rendered[contextfrag.RenderAuditManifest].Data.(*AuditManifestPayload)
	if !ok {
		t.Fatalf("audit payload = %T, want *AuditManifestPayload", got.Rendered[contextfrag.RenderAuditManifest].Data)
	}
	if len(payload.Manifest.EditTrace) != len(got.Manifest.EditTrace) {
		t.Fatalf("audit edit trace = %#v, want builder trace %#v", payload.Manifest.EditTrace, got.Manifest.EditTrace)
	}
	if len(payload.Manifest.DynamicMutators) != len(got.Manifest.DynamicMutators) ||
		len(payload.Manifest.DynamicMutators) != 1 ||
		payload.Manifest.DynamicMutators[0] != got.Manifest.DynamicMutators[0] {
		t.Fatalf("audit dynamic mutators = %#v, want %#v", payload.Manifest.DynamicMutators, got.Manifest.DynamicMutators)
	}
}

func testFrags() []contextfrag.ContextFrag {
	scope := contextfrag.Scope{BotID: "bot-1", SessionID: "session-1"}
	return []contextfrag.ContextFrag{
		contextfrag.TextFrag(contextfrag.TextFragInput{
			ID:         "system.prompt",
			Kind:       contextfrag.KindSystemPrompt,
			Role:       sdk.MessageRoleSystem,
			Slot:       contextfrag.SlotSystem,
			Text:       "System guidance",
			Priority:   10,
			CacheClass: contextfrag.CacheStable,
			Trust:      contextfrag.TrustSystem,
			Scope:      scope,
			Source:     "static",
			Collector:  "static",
			Render:     contextfrag.RenderPolicy{Format: contextfrag.RenderMarkdown},
		}),
		contextfrag.MessageFrag(contextfrag.MessageFragInput{
			ID:         "history.001",
			Message:    sdk.UserMessage("previous user turn"),
			Kind:       contextfrag.KindConversationEvent,
			Slot:       contextfrag.SlotHistory,
			Priority:   40,
			CacheClass: contextfrag.CacheDynamic,
			Trust:      contextfrag.TrustUser,
			Scope:      scope,
			Source:     "static",
			Collector:  "static",
			Index:      1,
		}),
		contextfrag.TextFrag(contextfrag.TextFragInput{
			ID:         "current.message",
			Kind:       contextfrag.KindCurrentUserMessage,
			Role:       sdk.MessageRoleUser,
			Slot:       contextfrag.SlotCurrentUser,
			Text:       "current request",
			Priority:   90,
			CacheClass: contextfrag.CacheNever,
			Trust:      contextfrag.TrustUser,
			Scope:      scope,
			Source:     "static",
			Collector:  "static",
			Render:     contextfrag.RenderPolicy{Format: contextfrag.RenderSDKMessage},
		}),
	}
}
