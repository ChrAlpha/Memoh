package native

import (
	"context"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	tools "github.com/memohai/memoh/internal/agent/tool"
)

type staticMCPProvider struct{ staticToolProvider }

func (staticMCPProvider) ProviderLabel() string { return "mcp" }

func TestAssembleToolsAccountsToolDefinitions(t *testing.T) {
	t.Parallel()

	nativeTool := sdk.Tool{
		Name:        "send_message",
		Description: "Send a message to the current conversation.",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
	}
	mcpTool := sdk.Tool{
		Name:        "jira_search",
		Description: "Search Jira issues via the connected MCP server.",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"jql": map[string]any{"type": "string"}}},
	}
	a := newTestAgent(
		staticToolProvider{tools: []sdk.Tool{nativeTool}},
		staticMCPProvider{staticToolProvider: staticToolProvider{tools: []sdk.Tool{mcpTool}}},
	)

	_, _, defs, err := a.assembleTools(context.Background(), RunConfig{}, tools.StreamEmitter(func(tools.ToolStreamEvent) {}), true)
	if err != nil {
		t.Fatalf("assembleTools error: %v", err)
	}
	want := []contextfrag.ToolDefAccounting{
		contextfrag.ToolDefAccountingFor("native", nativeTool),
		contextfrag.ToolDefAccountingFor("mcp", mcpTool),
	}
	if len(defs) != len(want) {
		t.Fatalf("tool defs = %+v, want %+v", defs, want)
	}
	for i := range want {
		if defs[i] != want[i] {
			t.Fatalf("tool defs[%d] = %+v, want %+v", i, defs[i], want[i])
		}
	}
	if defs[0].TokenEstimate == 0 || defs[1].TokenEstimate == 0 {
		t.Fatal("tool definition estimates must be nonzero")
	}
}

func TestRefreshContextFragStampsToolDefs(t *testing.T) {
	t.Parallel()

	defs := []contextfrag.ToolDefAccounting{{Provider: "native", Name: "send_message", Bytes: 100, TokenEstimate: 25}}
	cfg := RunConfig{System: "base system", ContextToolDefs: defs}.RefreshContextFrag()
	if len(cfg.ContextManifest.ToolDefs) != 1 || cfg.ContextManifest.ToolDefs[0] != defs[0] {
		t.Fatalf("manifest tool defs = %+v, want %+v", cfg.ContextManifest.ToolDefs, defs)
	}
}
