package contextview

import (
	"reflect"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	native "github.com/felinics/memoh/internal/agent/runtime/native"
)

func TestProviderViewCarriesSummaryCoverageToContextSwitch(t *testing.T) {
	covered := contextfrag.ContextRef{Namespace: "bot_history_message", ID: "old-row", ContentHash: "original", Durability: contextfrag.RefDurable}
	record := historyfrag.SummaryRecord("summary-id", "Earlier task progress", []contextfrag.ContextRef{covered}, contextfrag.Scope{})
	frag := historyfrag.ToFrag(record)
	frag.ID = "message.000"
	cfg := native.RunConfig{ContextSourceFrags: []contextfrag.ContextFrag{
		frag,
		currentMessageFrag("message.001", "continue the task"),
	}, ContextQueryMaterialized: true}
	got, err := ProviderRunConfigApplier(nil)(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ContextMessageSources) != len(got.Messages) || len(got.Messages) != 2 {
		t.Fatalf("sources = %d, messages = %d", len(got.ContextMessageSources), len(got.Messages))
	}
	if source := got.ContextMessageSources[0]; !source.Ref.EqualIdentity(record.Ref) || !reflect.DeepEqual(source.Coverage, []contextfrag.ContextRef{covered}) {
		t.Fatalf("preloaded summary lost its source coverage: %+v", source)
	}
	if got.ContextMessageSources[1].Ref.ID != "" || !reflect.DeepEqual(got.Messages[1], sdk.UserMessage("continue the task")) {
		t.Fatal("current input became eligible for historical summary replacement")
	}
}
