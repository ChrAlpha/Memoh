package external

import (
	"net/url"
	"os"
	"strings"
)

// ToolGatewayURLFromEnv is the tool gateway URL an External Agent runtime calls
// back into for a bot, as the deployment configures it: MEMOH_ACP_MCP_HTTP_URL,
// with an optional {bot_id} placeholder, or MEMOH_ACP_MCP_HTTP_BASE_URL. It is
// empty when neither is set.
func ToolGatewayURLFromEnv(botID string) string {
	if raw := strings.TrimSpace(os.Getenv("MEMOH_ACP_MCP_HTTP_URL")); raw != "" {
		if strings.Contains(raw, "{bot_id}") {
			return strings.ReplaceAll(raw, "{bot_id}", url.PathEscape(strings.TrimSpace(botID)))
		}
		return raw
	}
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("MEMOH_ACP_MCP_HTTP_BASE_URL")), "/")
	if base == "" {
		return ""
	}
	return base + "/bots/" + url.PathEscape(strings.TrimSpace(botID)) + "/tools"
}
