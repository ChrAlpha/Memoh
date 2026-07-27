package contextfrag

import (
	"encoding/json"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"
)

func TestToolDefAccountingForMeasuresSerializedDefinition(t *testing.T) {
	t.Parallel()

	tool := sdk.Tool{
		Name:        "send_message",
		Description: "Send a message to the current conversation.",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
	}
	got := ToolDefAccountingFor("native", tool)

	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	want := ToolDefAccounting{
		Provider:      "native",
		Name:          "send_message",
		Bytes:         len(data),
		TokenEstimate: TokensFromBytes(len(data)),
	}
	if got != want {
		t.Fatalf("ToolDefAccountingFor = %+v, want %+v", got, want)
	}
	if got.TokenEstimate == 0 {
		t.Fatal("tool definition estimate must be nonzero")
	}
}

func TestLifecycleSnapshotCarriesToolDefs(t *testing.T) {
	t.Parallel()

	manifest := Manifest{ToolDefs: []ToolDefAccounting{
		{Provider: "native", Name: "send_message", Bytes: 400, TokenEstimate: 100},
		{Provider: "mcp", Name: "jira_search", Bytes: 800, TokenEstimate: 200},
	}}
	snapshot := BuildLifecycleSnapshot(manifest)
	if len(snapshot.ToolDefs) != 2 || snapshot.ToolDefs[1].Provider != "mcp" {
		t.Fatalf("snapshot tool defs = %+v, want manifest's two entries", snapshot.ToolDefs)
	}
}

func TestLifecycleHolderClonesToolDefs(t *testing.T) {
	t.Parallel()

	manifest := Manifest{ToolDefs: []ToolDefAccounting{{Provider: "native", Name: "send_message", Bytes: 400, TokenEstimate: 100}}}
	holder := NewLifecycleHolder()
	holder.SetManifest(manifest)
	manifest.ToolDefs[0].TokenEstimate = -1
	snapshot, ok := holder.Snapshot()
	if !ok {
		t.Fatal("holder must produce a snapshot")
	}
	if snapshot.ToolDefs[0].TokenEstimate == -1 {
		t.Fatal("holder must clone tool defs, not share the caller's slice")
	}
}
