package command

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	dbsqlc "github.com/memohai/memoh/internal/db/postgres/sqlc"
)

func TestContextRegistered(t *testing.T) {
	t.Parallel()
	h := newTestHandler(nil)
	g, ok := h.registry.groups["context"]
	if !ok || g.DefaultAction != "show" {
		t.Fatalf("/context not registered with show default")
	}
}

func TestRenderProgressBar(t *testing.T) {
	t.Parallel()
	if got := renderProgressBar(0.5, 10); got != strings.Repeat("█", 5)+strings.Repeat("░", 5) {
		t.Errorf("bar 0.5 = %q", got)
	}
	if got := renderProgressBar(2, 4); got != strings.Repeat("█", 4) {
		t.Errorf("bar clamp high = %q", got)
	}
	if got := renderProgressBar(-1, 4); got != strings.Repeat("░", 4) {
		t.Errorf("bar clamp low = %q", got)
	}
}

func TestRenderContextUsageNoWindow(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithQueries(&fakeRoleResolver{role: "owner"}, &fakeCommandQueries{
		messageCount: 7, latestUsage: 1500,
	})
	out, err := h.renderContextUsage(CommandContext{Ctx: context.Background(), BotID: "b"}, "11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "**Context**") {
		t.Errorf("missing bold title: %s", out)
	}
	if !strings.Contains(out, "Messages: 7") {
		t.Errorf("missing message count: %s", out)
	}
	// No model service wired => no window => the "N tokens used" fallback path.
	if !strings.Contains(out, "1.5K tokens used") {
		t.Errorf("missing used tokens: %s", out)
	}
}

func TestRenderContextUsageBreakdownRows(t *testing.T) {
	t.Parallel()
	snapshot := contextfrag.LifecycleSnapshot{
		Breakdown: []contextfrag.KindBreakdown{
			{Kind: contextfrag.KindConversationEvent, Fragments: 12, TokenEstimate: 96200},
			{Kind: contextfrag.KindSystemPrompt, Fragments: 3, TokenEstimate: 5900},
			{Kind: contextfrag.KindSkillsCatalog, Fragments: 1, TokenEstimate: 2700},
		},
		ToolDefs: []contextfrag.ToolDefAccounting{
			{Provider: "native", Name: "send_message", Bytes: 96400, TokenEstimate: 24100},
			{Provider: "mcp", Name: "jira_search", Bytes: 6000, TokenEstimate: 1500},
		},
	}
	meta, err := json.Marshal(map[string]any{contextfrag.MetadataContextLifecycleKey: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	h := newTestHandlerWithQueries(&fakeRoleResolver{role: "owner"}, &fakeCommandQueries{
		messageCount: 7,
		latestUsage:  158300,
		lifecycleRows: []dbsqlc.ListRecentAssistantMessagesBySessionRow{{
			Metadata: meta,
		}},
	})
	out, err := h.renderContextUsage(CommandContext{Ctx: context.Background(), BotID: "b"}, "11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Conversation: ~96.2K",
		"Tool definitions: ~24.1K",
		"System prompt: ~5.9K",
		"Skills: ~2.7K",
		"MCP tools: ~1.5K",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderContextUsageWithoutSnapshotOmitsRows(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithQueries(&fakeRoleResolver{role: "owner"}, &fakeCommandQueries{
		messageCount: 2, latestUsage: 900,
	})
	out, err := h.renderContextUsage(CommandContext{Ctx: context.Background(), BotID: "b"}, "11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "~") {
		t.Errorf("no snapshot must render no estimate rows:\n%s", out)
	}
}
