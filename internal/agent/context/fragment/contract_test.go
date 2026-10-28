package contextfrag

import (
	"encoding/json"
	"reflect"
	"testing"

	sdk "github.com/felinics/twilight/sdk"
)

func TestContextRefRoundTripPreservesReducerIdentity(t *testing.T) {
	t.Parallel()

	ref := ContextRef{
		Namespace:  "history",
		ID:         "row-1",
		Version:    2,
		Range:      &ContentRange{Start: 10, End: 20},
		Schema:     SchemaContextRef,
		Durability: RefDurable,
	}

	raw, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("marshal context ref: %v", err)
	}
	var got ContextRef
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal context ref: %v", err)
	}

	if !reflect.DeepEqual(got, ref) || got.StableKey() != ref.StableKey() {
		t.Fatalf("roundtrip ref = %#v, want %#v", got, ref)
	}
	if err := ValidateContextRef(ContextRef{Namespace: "history", Schema: SchemaContextRef}); err == nil {
		t.Fatal("context ref without ID should fail validation")
	}
}

func TestDropPriorityHigherValuesDropFirst(t *testing.T) {
	t.Parallel()

	if !DropPriority(20).DropsBefore(10) {
		t.Fatal("higher drop priority must drop before a lower value")
	}
	if DropPriority(10).DropsBefore(20) {
		t.Fatal("lower drop priority must survive longer than a higher value")
	}
}

func TestMessageFragAndRepairPreserveSectionPolicy(t *testing.T) {
	t.Parallel()

	policy := RenderPolicy{Format: RenderSDKMessage, GroupID: "messages", GroupJoiner: "\n"}
	frag := MessageFrag(MessageFragInput{
		ID:                 "message.001",
		Message:            sdk.UserMessage("before"),
		Kind:               KindConversationEvent,
		Slot:               SlotHistory,
		RetentionTier:      RetentionPreferred,
		DropPriority:       25,
		RequiredCapability: "read",
		Render:             policy,
	})
	rebuilt := RebuildFragMessage(frag, sdk.AssistantMessage("after"))

	if rebuilt.RetentionTier != frag.RetentionTier || rebuilt.DropPriority != frag.DropPriority ||
		rebuilt.RequiredCapability != frag.RequiredCapability || rebuilt.Render != policy {
		t.Fatalf("rebuilt policy = %#v, want %#v", rebuilt, frag)
	}
}

func TestBuildManifestItemCarriesSectionPolicy(t *testing.T) {
	t.Parallel()

	render := RenderPolicy{Format: RenderMarkdown, GroupID: "system.skills", GroupJoiner: "\n"}
	frag := TextFrag(TextFragInput{
		ID:                 "system.skill.alpha",
		Kind:               KindSkillsCatalog,
		Slot:               SlotSystem,
		Text:               "alpha",
		RetentionTier:      RetentionOptional,
		DropPriority:       30,
		RequiredCapability: "use_skill",
		Render:             render,
	})
	manifest := BuildManifest([]ContextFrag{frag})

	if len(manifest.Items) != 1 {
		t.Fatalf("manifest items = %d, want 1", len(manifest.Items))
	}
	item := manifest.Items[0]
	if item.RetentionTier != RetentionOptional || item.DropPriority != 30 ||
		item.RequiredCapability != "use_skill" || item.Render != render {
		t.Fatalf("manifest section policy = %#v", item)
	}
}

func TestSerdeRoundTripsContextFragManifestAndContinuity(t *testing.T) {
	t.Parallel()

	frag := TextFrag(TextFragInput{
		ID:         "history.001",
		Kind:       KindConversationEvent,
		Role:       sdk.MessageRoleUser,
		Slot:       SlotHistory,
		Text:       "hello",
		Priority:   70,
		CacheClass: CacheNever,
		Trust:      TrustExternal,
		Source:     "history",
		SourceID:   "row-1",
		Collector:  "test",
	})
	frag = WithContextRef(frag, ContextRef{
		Namespace: "history",
		ID:        "row-1",
		Version:   1,
		Schema:    SchemaContextRef,
	})

	roundTripJSON(t, frag, func(got ContextFrag) {
		if got.Ref != frag.Ref {
			t.Fatalf("frag ref mismatch after roundtrip: got %#v want %#v", got.Ref, frag.Ref)
		}
	})
	msgFrag := WithContextRef(MessageFrag(MessageFragInput{
		ID:         "message.001",
		Message:    sdk.UserMessage("from sdk", sdk.ImagePart{Image: "data:image/png;base64,abc", MediaType: "image/png"}),
		Kind:       KindConversationEvent,
		Slot:       SlotHistory,
		Priority:   70,
		CacheClass: CacheNever,
		Trust:      TrustExternal,
		Source:     "history",
		SourceID:   "row-2",
		Collector:  "test",
	}), ContextRef{
		Namespace: "history",
		ID:        "row-2",
		Version:   1,
		Schema:    SchemaContextRef,
	})
	roundTripJSON(t, msgFrag, func(got ContextFrag) {
		rendered := Render([]ContextFrag{got})
		if len(rendered.Messages) != 1 {
			t.Fatalf("round-tripped sdk message fragment rendered %d messages, want 1", len(rendered.Messages))
		}
		if rendered.Messages[0].Role != sdk.MessageRoleUser {
			t.Fatalf("round-tripped sdk message role = %q, want user", rendered.Messages[0].Role)
		}
		if !reflect.DeepEqual(rendered.Messages[0], *msgFrag.Parts[0].Message) {
			t.Fatalf("round-tripped sdk message mismatch: got %#v want %#v", rendered.Messages[0], msgFrag.Parts[0].Message)
		}
	})

	manifest := BuildManifest([]ContextFrag{frag, msgFrag})
	roundTripJSON(t, manifest, func(got Manifest) {
		if !hasSchemaVersion(got.SchemaVersions, SchemaContextManifest, CurrentSchemaVersion) {
			t.Fatalf("manifest schema versions missing manifest version: %#v", got.SchemaVersions)
		}
		if len(got.Items) != 2 || got.Items[0].Ref != frag.Ref {
			t.Fatalf("manifest item ref mismatch: %#v", got.Items)
		}
		if len(got.SlotPolicies) == 0 {
			t.Fatal("manifest should include per-slot render policies")
		}
		if got.Counts.Images != 1 {
			t.Fatalf("manifest image count = %d, want 1", got.Counts.Images)
		}
	})

	group := ContinuityGroup{
		ID:               "tool-call-1",
		Kind:             "tool_call",
		Provider:         "openai",
		ModelFamily:      "responses",
		Refs:             []ContextRef{frag.Ref},
		MustKeepTogether: true,
		MustKeepRaw:      true,
		MustKeepOrder:    true,
		MustBeComplete:   true,
	}
	roundTripJSON(t, group, func(got ContinuityGroup) {
		if len(got.Refs) != 1 || got.Refs[0] != frag.Ref || !got.MustKeepTogether || !got.MustBeComplete {
			t.Fatalf("continuity group mismatch after roundtrip: %#v", got)
		}
	})
}

func TestSchemaValidationRejectsSilentDrift(t *testing.T) {
	t.Parallel()

	if err := ValidateSchemaVersions([]SchemaVersion{{Name: SchemaContextFrag, Version: 999}}); err == nil {
		t.Fatal("unknown context frag schema version should fail validation")
	}
	if err := ValidateSchemaVersions([]SchemaVersion{{Name: "unknown_schema", Version: CurrentSchemaVersion}}); err == nil {
		t.Fatal("unknown schema name should fail validation")
	}
	if !UsesLockstepSchemaVersions() {
		t.Fatal("PR0 schema strategy should explicitly report lockstep schema versions")
	}
	for _, schema := range DefaultSchemaVersions() {
		if schema.Version != CurrentSchemaVersion {
			t.Fatalf("lockstep schema %q version = %d, want %d", schema.Name, schema.Version, CurrentSchemaVersion)
		}
	}
}

// A ref names what a fragment renders: the identity its caller gives,
// else the source its provenance names, else the fragment's own ID. Only a
// fragment with a source is durable.
func TestWithContextRefNamesAnIdentity(t *testing.T) {
	t.Parallel()

	frag := TextFrag(TextFragInput{ID: "history.001", Kind: KindConversationEvent, Slot: SlotHistory, Text: "hello", Source: "history", SourceID: "row-1"})
	for _, tc := range []struct {
		name       string
		frag       ContextFrag
		ref        ContextRef
		id         string
		durability RefDurability
	}{
		{name: "explicit", frag: frag, ref: ContextRef{ID: "summary-1"}, id: "summary-1", durability: RefDurable},
		{name: "source", frag: frag, id: "row-1", durability: RefDurable},
		{name: "own id", frag: TextFrag(TextFragInput{ID: "system.intro", Kind: KindSystemPrompt, Slot: SlotSystem, Text: "intro"}), id: "system.intro", durability: RefSynthetic},
	} {
		got := WithContextRef(tc.frag, tc.ref).Ref
		if got.ID != tc.id || got.Durability != tc.durability || ValidateContextRef(got) != nil {
			t.Fatalf("%s: ref = %#v, want id %q durability %q", tc.name, got, tc.id, tc.durability)
		}
	}
	edited := RebuildFragMessage(MessageFrag(MessageFragInput{ID: "message.001", Message: sdk.UserMessage("before"), Source: "history", SourceID: "row-2"}), sdk.UserMessage("after"))
	if edited.Ref.ID != "row-2" {
		t.Fatalf("an edited fragment keeps its identity: %#v", edited.Ref)
	}
}

func roundTripJSON[T any](t *testing.T, input T, check func(T)) {
	t.Helper()

	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal %T: %v", input, err)
	}
	var got T
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %T: %v", input, err)
	}
	check(got)
}

func hasSchemaVersion(versions []SchemaVersion, name string, version int) bool {
	for _, got := range versions {
		if got.Name == name && got.Version == version {
			return true
		}
	}
	return false
}

func renderedOutputContainsRef(outputs []RenderedOutputRef, ref ContextRef) bool {
	for _, output := range outputs {
		for _, got := range output.Refs {
			if got.StableKey() == ref.StableKey() {
				return true
			}
		}
	}
	return false
}

func manifestHasWarning(manifest Manifest, code string) bool {
	return countManifestWarnings(manifest, code) > 0
}

func countManifestWarnings(manifest Manifest, code string) int {
	total := 0
	for _, warning := range manifest.ValidationWarnings {
		if warning.Code == code {
			total++
		}
	}
	return total
}
