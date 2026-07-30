package application

import (
	"context"
	"slices"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/agent/runtime/native"
	"github.com/memohai/memoh/internal/contextview"
	"github.com/memohai/memoh/internal/hooks"
)

func TestBuildHookSystemSectionsMapsPolicyAndResolvesIDs(t *testing.T) {
	t.Parallel()

	outputs := []promptHookOutput{
		{
			Event: hooks.EventBeforePromptBuild,
			Result: hooks.Result{
				AppendSystemSections: []hooks.SystemSectionOutput{
					{
						HookName: "alpha", Text: "anonymous",
						Retention: hooks.SystemSectionRetentionOptional,
						Cache:     hooks.SystemSectionCacheDynamic,
					},
					{
						HookName: "alpha", ID: "x", Text: "first x",
						Retention: hooks.SystemSectionRetentionPreferred,
						Cache:     hooks.SystemSectionCacheStable,
						WarningCodes: []string{
							hooks.WarningSystemSectionRequiredClamped,
						},
					},
					{
						HookName: "alpha", ID: "x", Text: "second x",
						Retention: hooks.SystemSectionRetentionOptional,
						Cache:     hooks.SystemSectionCacheDynamic,
					},
					{
						HookName: "alpha", ID: "x.2", Text: "explicit suffix",
						Retention: hooks.SystemSectionRetentionOptional,
						Cache:     hooks.SystemSectionCacheDynamic,
					},
					{
						HookName: "alpha.x", Text: "structural collision",
						Retention: hooks.SystemSectionRetentionOptional,
						Cache:     hooks.SystemSectionCacheDynamic,
					},
				},
				Warnings: []hooks.OutputWarning{{
					Code:     hooks.WarningInvalidAppendSystemSection,
					HookName: "alpha",
				}},
			},
		},
	}

	build := buildHookSystemSections(outputs, contextfrag.Scope{BotID: "bot-1"})
	gotIDs := make([]string, 0, len(build.Frags))
	for _, frag := range build.Frags {
		gotIDs = append(gotIDs, frag.ID)
		if frag.Kind != contextfrag.KindHookContext ||
			frag.Role != sdk.MessageRoleSystem ||
			frag.Slot != contextfrag.SlotSystem ||
			frag.Trust != contextfrag.TrustWorkspace ||
			frag.Priority != hookSystemSectionPriority ||
			frag.Budget.MaxChars != maxHookSystemSectionChars ||
			frag.Budget.Overflow != contextfrag.OverflowTrim {
			t.Fatalf("hook fragment shape = %#v", frag)
		}
	}
	wantIDs := []string{
		"system.hook.alpha",
		"system.hook.alpha.x",
		"system.hook.alpha.x.2",
		"system.hook.alpha.x.2.2",
		"system.hook.alpha.x.3",
	}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("hook fragment IDs = %v, want collision-safe IDs %v", gotIDs, wantIDs)
	}

	preferred := build.Frags[1]
	if preferred.RetentionTier != contextfrag.RetentionPreferred || preferred.CacheClass != contextfrag.CacheStable {
		t.Fatalf("preferred/stable policy not mapped: %#v", preferred)
	}
	if build.Frags[0].RetentionTier != contextfrag.RetentionOptional ||
		build.Frags[0].CacheClass != contextfrag.CacheDynamic {
		t.Fatalf("optional/dynamic defaults not mapped: %#v", build.Frags[0])
	}
	if len(build.Warnings) != 2 {
		t.Fatalf("warnings = %#v, want clamp and invalid-shape warnings", build.Warnings)
	}
	if build.Warnings[0].Code != hooks.WarningSystemSectionRequiredClamped ||
		build.Warnings[0].Ref.ID != preferred.Ref.ID {
		t.Fatalf("clamp warning = %#v, want collision-resolved fragment ref %#v", build.Warnings[0], preferred.Ref)
	}
	if build.Warnings[1].Code != hooks.WarningInvalidAppendSystemSection ||
		build.Warnings[1].Ref.ID != "" {
		t.Fatalf("invalid declaration warning = %#v, want content-light warning without ref", build.Warnings[1])
	}
}

func TestHookSystemSectionsSitBetweenBuiltinsAndNonSystemSources(t *testing.T) {
	t.Parallel()

	scope := contextfrag.Scope{BotID: "bot-1"}
	builtins := native.SystemSectionFrags(native.GenerateSystemSections(native.SystemPromptParams{
		Files: []native.SystemFile{{Filename: "AGENTS.md", Content: "workspace guidance"}},
	}), scope)
	hookBuild := buildHookSystemSections([]promptHookOutput{{
		Event: hooks.EventBeforePromptBuild,
		Result: hooks.Result{AppendSystemSections: []hooks.SystemSectionOutput{{
			HookName: "policy", ID: "guardrail", Text: "hook system guidance",
			Retention: hooks.SystemSectionRetentionPreferred,
			Cache:     hooks.SystemSectionCacheStable,
		}}},
	}}, scope)
	nonSystem := contextview.CollectNonSystemProviderSourceFrags(context.Background(), native.RunConfig{
		Messages:     []sdk.Message{sdk.UserMessage("history")},
		ContextScope: scope,
	})
	frags := append(append(builtins, hookBuild.Frags...), nonSystem...)

	hookIndex, historyIndex, lastBuiltinIndex := -1, -1, len(builtins)-1
	for i, frag := range frags {
		if frag.ID == "system.hook.policy.guardrail" {
			hookIndex = i
		}
		if frag.Slot == contextfrag.SlotHistory && historyIndex < 0 {
			historyIndex = i
		}
	}
	if hookIndex <= lastBuiltinIndex || historyIndex <= hookIndex {
		t.Fatalf("source order builtins=%d hook=%d history=%d", lastBuiltinIndex, hookIndex, historyIndex)
	}
}

func TestAfterPromptHookSystemBytesIncludesBothHookChannels(t *testing.T) {
	t.Parallel()

	system := "base system"
	turnContext := formatServiceHookContext(hooks.EventBeforePromptBuild, "turn-only")
	result := hooks.Result{AppendSystemSections: []hooks.SystemSectionOutput{
		{Text: "system a"},
		{Text: "system b"},
	}}
	hookTexts := append([]string{turnContext}, hookSystemSectionTexts(result)...)

	got := afterPromptHookSystemBytes(system, hookTexts)
	want := len(system) + len("\n\n") + len(strings.Join(hookTexts, "\n\n"))
	if got != want {
		t.Fatalf("system bytes = %d, want %d for both hook channels", got, want)
	}
}
