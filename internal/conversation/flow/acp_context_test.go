package flow

import (
	"context"
	"strings"
	"testing"
	"time"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/contextview"
	"github.com/memohai/memoh/internal/conversation"
)

func TestRenderACPContextMarkdownIncludesDynamicRuntimeAndMemory(t *testing.T) {
	t.Parallel()

	got := acpMarkdownViaSections(t, acpContextRenderInput{
		Now:                     time.Date(2026, 6, 1, 9, 30, 0, 0, time.FixedZone("PDT", -7*3600)),
		Timezone:                "America/Los_Angeles",
		BotID:                   "bot-1",
		SessionID:               "session-1",
		AgentID:                 "codex",
		ProjectPath:             "/data/app",
		DisplayName:             "Alice",
		CurrentChannel:          "telegram",
		ConversationType:        "group",
		ConversationName:        "Dev Group",
		SourceChannelIdentityID: "identity-1",
		Attachments: []conversation.ChatAttachment{{
			Name: "spec.md",
			Path: "/data/uploads/spec.md",
			Mime: "text/markdown",
		}},
		Files: []agentpkg.SystemFile{
			{Filename: "IDENTITY.md", Content: "I am Memo."},
			{Filename: "SOUL.md", Content: "Be concise."},
			{Filename: "TOOLS.md", Content: "Do not inject normal tool prompt."},
			{Filename: "MEMORY.md", Content: "User prefers small patches."},
			{Filename: "PROFILES.md", Content: "Alice is the project owner."},
			{Filename: "memory/preference/alice-profile.md", Content: "Alice prefers small, reviewable patches."},
		},
	})

	for _, want := range []string{
		"# Memoh ACP Context",
		"Current time: 2026-06-01T09:30:00-07:00",
		"Timezone: America/Los_Angeles",
		"Bot ID: bot-1",
		"ACP agent: codex",
		"Workspace: /data/app",
		"Sender: Alice",
		"Conversation name: Dev Group",
		"name=spec.md",
		"## Bot Identity",
		"Embedded excerpt from `/data/IDENTITY.md`",
		"I am Memo.",
		"## Bot Soul",
		"Be concise.",
		"## Long-Term Memory",
		"User prefers small patches.",
		"## Profiles",
		"Alice is the project owner.",
		"## Memory Concept - preference/alice-profile.md",
		"Alice prefers small, reviewable patches.",
		"This virtual resource is already embedded",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("context missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Do not inject normal tool prompt.") {
		t.Fatalf("TOOLS.md content should not be injected into ACP context:\n%s", got)
	}
}

func TestRenderACPContextMarkdownRespectsSystemFilesBudget(t *testing.T) {
	t.Parallel()

	large := "HEAD\n" + strings.Repeat("0123456789", 200) + "\nTAIL"
	got := acpMarkdownViaSections(t, acpContextRenderInput{
		Now:                 time.Date(2026, 6, 1, 9, 30, 0, 0, time.UTC),
		Timezone:            "UTC",
		BotID:               "bot-1",
		SessionID:           "session-1",
		AgentID:             "codex",
		ProjectPath:         "/data/app",
		SystemFilesMaxBytes: 512,
		Files: []agentpkg.SystemFile{
			{Filename: "MEMORY.md", Content: large},
			{Filename: "PROFILES.md", Content: "SECOND_FILE_SHOULD_NOT_FIT"},
		},
	})

	if !strings.Contains(got, "[memoh pruned]") {
		t.Fatalf("context missing prune marker:\n%s", got)
	}
	if strings.Contains(got, "SECOND_FILE_SHOULD_NOT_FIT") {
		t.Fatalf("context included system file content beyond budget:\n%s", got)
	}
}

func acpMarkdownViaSections(t *testing.T, input acpContextRenderInput) string {
	t.Helper()
	markdown, uri, _ := acpContextViaContextView(context.Background(), nil, buildACPContextSections(input), "")
	if uri != acpContextURI {
		t.Fatalf("uri = %q, want %q", uri, acpContextURI)
	}
	return markdown
}

func TestACPContextViaContextViewKeepsQueryOutsideMarkdown(t *testing.T) {
	t.Parallel()

	sections := []contextview.ACPSection{
		{ID: "acp.preamble", Text: "# Memoh ACP Context\n\npreamble body"},
		{ID: "acp.section.current-runtime", Text: "## Current Runtime\n\n- Bot ID: bot-1"},
	}
	baseMarkdown, _, _ := acpContextViaContextView(context.Background(), nil, sections, "")
	markdown, uri, _ := acpContextViaContextView(context.Background(), nil, sections, "deploy the fix")

	if strings.Contains(markdown, "deploy the fix") {
		t.Fatalf("query must not join the context document: %q", markdown)
	}
	if markdown != baseMarkdown {
		t.Fatalf("query must not change context markdown bytes:\n got: %q\nbase: %q", markdown, baseMarkdown)
	}
	if uri != acpContextURI {
		t.Fatalf("uri = %q, want %q", uri, acpContextURI)
	}
}

func TestRenderACPMetadataSectionGolden(t *testing.T) {
	t.Parallel()

	got := renderACPMetadataSection([][2]string{
		{"Current time", "2026-06-01T09:30:00Z"},
		{"Timezone", "UTC"},
		{"Empty value", ""},
		{"", "orphan"},
		{" Spaced key ", " spaced value "},
	})

	want := "- Current time: 2026-06-01T09:30:00Z\n" +
		"- Timezone: UTC\n" +
		"- Spaced key: spaced value"
	if got != want {
		t.Fatalf("metadata section bytes changed:\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderACPAttachmentsSectionGolden(t *testing.T) {
	t.Parallel()

	got := renderACPAttachmentsSection([]conversation.ChatAttachment{
		{
			Name:        "spec.md",
			Type:        "file",
			Mime:        "text/markdown",
			Path:        "/data/uploads/spec.md",
			URL:         "https://example.com/spec.md",
			ContentHash: "abc123",
			Size:        42,
		},
		{Path: "/tmp/img.png"},
	})

	want := "- Attachment 1, name=spec.md, type=file, mime=text/markdown, path=/data/uploads/spec.md, url=https://example.com/spec.md, content_hash=abc123, size=42\n" +
		"- Attachment 2, path=/tmp/img.png"
	if got != want {
		t.Fatalf("attachments section bytes changed:\n got: %q\nwant: %q", got, want)
	}

	if empty := renderACPAttachmentsSection(nil); empty != "" {
		t.Fatalf("empty attachments must render empty, got %q", empty)
	}
}

func TestRenderACPFileSectionGolden(t *testing.T) {
	t.Parallel()

	got := renderACPFileSection("MEMORY.md", "notes\n```go\ncode\n```\ndone")

	want := "Embedded excerpt from `/data/MEMORY.md`. This content is already loaded; do not search for or read this file unless the user explicitly asks.\n\n" +
		"````markdown\nnotes\n```go\ncode\n```\ndone\n````"
	if got != want {
		t.Fatalf("file section bytes changed:\n got: %q\nwant: %q", got, want)
	}
}

func TestBuildACPContextSectionsAssignsMetadata(t *testing.T) {
	t.Parallel()

	sections := buildACPContextSections(acpContextRenderInput{
		BotID:       "bot-1",
		DisplayName: "Alice",
		Attachments: []conversation.ChatAttachment{{Name: "report.pdf"}},
		Files: []agentpkg.SystemFile{
			{Filename: "SOUL.md", Content: "the soul"},
		},
	})

	byID := make(map[string]contextview.ACPSection, len(sections))
	for _, section := range sections {
		byID[section.ID] = section
	}

	preamble := byID["acp.preamble"]
	if preamble.Budget.Overflow != contextfrag.OverflowKeep || preamble.CacheClass != contextfrag.CacheStable {
		t.Fatalf("preamble must be keep+stable: %+v", preamble)
	}
	runtime := byID["acp.section.current-runtime"]
	if runtime.CacheClass != contextfrag.CacheNever {
		t.Fatalf("runtime section is per-turn volatile: %+v", runtime)
	}
	attachments := byID["acp.section.attachments"]
	if attachments.Trust != contextfrag.TrustExternal || attachments.Kind != contextfrag.KindAttachmentRef {
		t.Fatalf("attachments describe external input: %+v", attachments)
	}
	file := byID["acp.section.file.000"]
	if file.Trust != contextfrag.TrustWorkspace || file.Kind != contextfrag.KindWorkspaceInstruction {
		t.Fatalf("workspace file sections carry workspace trust: %+v", file)
	}
	notes := byID["acp.section.runtime-notes"]
	if notes.Kind != contextfrag.KindSystemPolicy || notes.CacheClass != contextfrag.CacheStable {
		t.Fatalf("runtime notes are static policy: %+v", notes)
	}
}
