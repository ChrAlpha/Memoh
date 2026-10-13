package contextview

import (
	"reflect"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
)

func TestOptionalHistoryImagesRespectNoticeAndOriginalSources(t *testing.T) {
	image := func(data string) sdk.ImagePart { return sdk.ImagePart{Image: "data:image/png;base64," + data} }
	album := stableHistoryMessageFrag("album", sdk.UserMessage("album", image("MQ=="), image("Mg=="), image("Mw==")))
	answered := stableHistoryMessageFrag("answered", sdk.UserMessage(strings.Repeat("answered history", 100)))
	current := currentMessageFrag("current", "new input")
	original := []contextfrag.ContextFrag{answered, album, current}
	before := contextfrag.FragMessage(album)
	notice := contextfrag.ResolveProviderBudgetFragTokens(TrimNoticeFrag(contextfrag.Scope{}))
	budget := contextfrag.ResolveProviderBudgetFragTokens(album) + notice
	for _, delta := range []int{0, -1} {
		got, edits := shedOptionalHistoryImages(original, []string{"missing", "album"}, budget+delta)
		wantParts, wantEdits := 4, 0
		if delta < 0 {
			wantParts, wantEdits = 3, 1
		}
		if len(contextfrag.FragMessage(got[1]).Content) != wantParts || len(edits) != wantEdits {
			t.Fatalf("delta=%d edits=%v parts=%v", delta, edits, contextfrag.FragMessage(got[1]).Content)
		}
		if !reflect.DeepEqual(contextfrag.FragMessage(original[1]), before) || len(before.Content) != 4 || !reflect.DeepEqual(got[2], current) {
			t.Fatal("source or current input mutated")
		}
		if delta < 0 && (got[1].Ref.ContentHash == album.Ref.ContentHash || edits[0].trace.Refs[0] != got[1].Ref) {
			t.Fatal("edit lost rebuilt content identity")
		}
	}
}

func TestImageOmissionAuditSurvivesLaterTextDrop(t *testing.T) {
	cfg := recoveryBudgetFixture()
	frag := stableHistoryMessageFrag("history", sdk.UserMessage(strings.Repeat("text", 2000), sdk.ImagePart{Image: "data:image/png;base64,a"}))
	frag.Provenance.SourceID = "older"
	cfg.ContextSourceFrags[1] = frag
	cfg.ContextOptionalImageFrags = []string{"history"}
	got, err := ProviderRunConfigApplier(nil)(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	foundEdit, foundMutation := false, false
	for _, edit := range got.ContextManifest.EditTrace {
		foundEdit = foundEdit || edit.EditID == "images.budget_omit.history"
	}
	for _, mutation := range got.ContextMutations.Records() {
		foundMutation = foundMutation || mutation.Kind == contextfrag.MutationCurrentInputImagesOmitted && mutation.Detail == "sources=older"
	}
	if !foundEdit || !foundMutation {
		t.Fatalf("missing image degradation audit: edit=%v records=%v", foundEdit, got.ContextMutations.Records())
	}
}
