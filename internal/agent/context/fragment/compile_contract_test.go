package contextfrag

import (
	"testing"

	sdk "github.com/felinics/twilight/sdk"
)

func TestCompileAddsRefsAndManifestTraceWithoutChangingRenderedLegacyView(t *testing.T) {
	t.Parallel()

	system := "base system"
	messages := []sdk.Message{sdk.UserMessage("history")}
	images := []sdk.ImagePart{{Image: "data:image/png;base64,abc", MediaType: "image/png"}}

	got := Compile(CompileInput{
		Source:       "test",
		System:       system,
		Messages:     messages,
		Query:        "current",
		InlineImages: images,
	})

	if got.System != system || got.Query != "current" || len(got.Messages) != 1 || len(got.InlineImages) != 1 {
		t.Fatalf("rendered legacy view changed: system=%q query=%q messages=%d images=%d", got.System, got.Query, len(got.Messages), len(got.InlineImages))
	}
	for _, frag := range got.Frags {
		if err := ValidateContextRef(frag.Ref); err != nil {
			t.Fatalf("compiled frag %q has invalid ref %#v: %v", frag.ID, frag.Ref, err)
		}
	}
	if len(got.Manifest.RenderedOutputs) == 0 {
		t.Fatal("manifest should explain rendered output refs")
	}
	if !manifestHasWarning(got.Manifest, "non_durable_context_ref") {
		t.Fatalf("source-less legacy refs should produce synthetic warning: %#v", got.Manifest.ValidationWarnings)
	}
}

func TestContextRefEmptyDurabilityMatchesDurableIdentity(t *testing.T) {
	t.Parallel()

	legacy := ContextRef{Namespace: "history", ID: "row-1", Schema: SchemaContextRef}
	durable := legacy
	durable.Durability = RefDurable

	if legacy.StableKey() != durable.StableKey() {
		t.Fatalf("empty durability stable key = %q, durable = %q", legacy.StableKey(), durable.StableKey())
	}
	if legacy.StableKey() != "history:row-1#context_ref" {
		t.Fatalf("durable stable key should preserve legacy shape, got %q", legacy.StableKey())
	}
	if !legacy.IsDurable() {
		t.Fatalf("empty durability should default to durable for compatibility: %#v", legacy)
	}
}

func TestBuildManifestWarnsForDirectNonDurableRefs(t *testing.T) {
	t.Parallel()

	synthetic := TextFrag(TextFragInput{
		ID:   "history.synthetic",
		Kind: KindConversationEvent,
		Slot: SlotHistory,
		Text: "source-less",
	})
	debug := TextFrag(TextFragInput{
		ID:   "history.debug",
		Kind: KindConversationEvent,
		Slot: SlotHistory,
		Text: "debug",
	})
	debug.Ref = ContextRef{
		Namespace:  "test",
		ID:         "debug-ref",
		Schema:     SchemaContextRef,
		Durability: RefDebug,
	}

	manifest := BuildManifest([]ContextFrag{synthetic, debug})
	if countManifestWarnings(manifest, "non_durable_context_ref") != 2 {
		t.Fatalf("BuildManifest should warn for direct synthetic/debug refs: %#v", manifest.ValidationWarnings)
	}
}

func TestManifestRenderedOutputsOnlyIncludeRenderedRefs(t *testing.T) {
	t.Parallel()

	historyText := WithContextRef(TextFrag(TextFragInput{
		ID:   "history.text",
		Kind: KindConversationEvent,
		Slot: SlotHistory,
		Text: "not rendered by legacy renderer",
	}), ContextRef{Namespace: "test", ID: "history.text", Schema: SchemaContextRef})
	currentA := WithContextRef(TextFrag(TextFragInput{
		ID:   "current.a",
		Kind: KindCurrentUserMessage,
		Slot: SlotCurrentUser,
		Text: "a",
	}), ContextRef{Namespace: "test", ID: "current.a", Schema: SchemaContextRef})
	currentB := WithContextRef(TextFrag(TextFragInput{
		ID:   "current.b",
		Kind: KindCurrentUserMessage,
		Slot: SlotCurrentUser,
		Text: "b",
	}), ContextRef{Namespace: "test", ID: "current.b", Schema: SchemaContextRef})

	manifest := BuildManifest([]ContextFrag{historyText, currentA, currentB})
	if renderedOutputContainsRef(manifest.RenderedOutputs, historyText.Ref) {
		t.Fatalf("history text ref should not be listed as rendered output: %#v", manifest.RenderedOutputs)
	}
	if renderedOutputContainsRef(manifest.RenderedOutputs, currentA.Ref) {
		t.Fatalf("overwritten current-user text ref should not be listed as rendered output: %#v", manifest.RenderedOutputs)
	}
	if !renderedOutputContainsRef(manifest.RenderedOutputs, currentB.Ref) {
		t.Fatalf("last current-user text ref should be listed as rendered output: %#v", manifest.RenderedOutputs)
	}
}

func TestCompilePreservesExplicitMessageFragAtSourcePosition(t *testing.T) {
	t.Parallel()

	before := sdk.UserMessage("before")
	summary := sdk.UserMessage("<summary>\ncondensed\n</summary>")
	summaryRef := ContextRef{Namespace: "compaction_log", ID: "log-1", Schema: SchemaContextRef, Durability: RefDurable}
	summaryFrag := MessageFrag(MessageFragInput{
		ID:      "message.001",
		Message: summary,
		Kind:    KindConversationSummary,
		Slot:    SlotHistory,
		Source:  "compaction_log",
	})
	summaryFrag.Ref = summaryRef

	assembled := Compile(CompileInput{
		Source:   SourceRunConfig,
		Messages: []sdk.Message{before, summary},
		Existing: []ContextFrag{summaryFrag},
	})

	if len(assembled.Messages) != 2 {
		t.Fatalf("messages = %d, want explicit message frag to replace derived message only: %#v", len(assembled.Messages), assembled.Messages)
	}
	if got := assembled.Messages[0].Content[0].(sdk.TextPart).Text; got != "before" {
		t.Fatalf("first message = %q, want source order preserved", got)
	}
	if len(assembled.Manifest.Items) != 2 {
		t.Fatalf("manifest items = %d, want 2: %#v", len(assembled.Manifest.Items), assembled.Manifest.Items)
	}
	if assembled.Manifest.Items[1].Kind != KindConversationSummary {
		t.Fatalf("second manifest item kind = %q, want %q", assembled.Manifest.Items[1].Kind, KindConversationSummary)
	}
	if assembled.Manifest.Items[1].Ref.StableKey() != summaryRef.StableKey() {
		t.Fatalf("explicit message ref was not preserved: %#v", assembled.Manifest.Items[1].Ref)
	}
}

func TestCompilePreservesUnmatchedExplicitMessageFrag(t *testing.T) {
	t.Parallel()

	summary := sdk.UserMessage("<summary>\ncondensed\n</summary>")
	summaryRef := ContextRef{Namespace: "compaction_log", ID: "log-1", Schema: SchemaContextRef, Durability: RefDurable}
	summaryFrag := MessageFrag(MessageFragInput{
		ID:      "summary.log-1",
		Message: summary,
		Kind:    KindConversationSummary,
		Slot:    SlotHistory,
		Source:  "compaction_log",
	})
	summaryFrag.Ref = summaryRef

	assembled := Compile(CompileInput{
		Source:   SourceRunConfig,
		Messages: []sdk.Message{sdk.UserMessage("after summary")},
		Existing: []ContextFrag{summaryFrag},
	})

	if len(assembled.Messages) != 2 {
		t.Fatalf("messages = %d, want explicit summary plus derived message: %#v", len(assembled.Messages), assembled.Messages)
	}
	if len(assembled.Manifest.Items) != 2 {
		t.Fatalf("manifest items = %d, want explicit summary plus derived message: %#v", len(assembled.Manifest.Items), assembled.Manifest.Items)
	}
	if assembled.Manifest.Items[0].Kind != KindConversationSummary {
		t.Fatalf("first manifest item kind = %q, want %q", assembled.Manifest.Items[0].Kind, KindConversationSummary)
	}
	if assembled.Manifest.Items[0].Ref.StableKey() != summaryRef.StableKey() {
		t.Fatalf("unmatched explicit message ref was not preserved: %#v", assembled.Manifest.Items[0].Ref)
	}
}
