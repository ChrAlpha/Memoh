package external

import "testing"

func TestToolGatewayURLFromEnv(t *testing.T) {
	t.Setenv("MEMOH_ACP_MCP_HTTP_URL", "")
	t.Setenv("MEMOH_ACP_MCP_HTTP_BASE_URL", "")
	if got := ToolGatewayURLFromEnv("bot 1"); got != "" {
		t.Fatalf("unconfigured = %q, want empty", got)
	}

	t.Setenv("MEMOH_ACP_MCP_HTTP_BASE_URL", "https://memoh.example/api/")
	if got, want := ToolGatewayURLFromEnv("bot 1"), "https://memoh.example/api/bots/bot%201/tools"; got != want {
		t.Fatalf("base URL = %q, want %q", got, want)
	}

	t.Setenv("MEMOH_ACP_MCP_HTTP_URL", "https://tools.example/{bot_id}/mcp")
	if got, want := ToolGatewayURLFromEnv("bot 1"), "https://tools.example/bot%201/mcp"; got != want {
		t.Fatalf("templated URL = %q, want %q", got, want)
	}

	t.Setenv("MEMOH_ACP_MCP_HTTP_URL", "https://tools.example/mcp")
	if got, want := ToolGatewayURLFromEnv("bot 1"), "https://tools.example/mcp"; got != want {
		t.Fatalf("fixed URL = %q, want %q", got, want)
	}
}
