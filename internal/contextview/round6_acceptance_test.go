package contextview

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
	"github.com/memohai/memoh/internal/agent/sessionmode"
)

var round6NativeModes = []string{
	sessionmode.Chat,
	sessionmode.Discuss,
	sessionmode.Heartbeat,
	sessionmode.Schedule,
	sessionmode.Subagent,
}

func TestNativeModeSystemBudgetPressureAndDiscussException(t *testing.T) {
	t.Parallel()

	for _, mode := range round6NativeModes {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			scope := contextfrag.Scope{BotID: "bot-1", SessionID: "session-1"}
			base := round6StaticSystemFrags(mode, scope)
			usage := strings.Repeat("registered tool guidance 猫😺 ", 400)
			marker := systemBudgetMarkerFrag([]string{"system.tool_usage"}, scope)
			window := contextWindowForDefaultOutputReserve(systemFragCost(appendClone(base, marker)))

			cfg := agentpkg.RunConfig{
				SessionType:            mode,
				ContextScope:           scope,
				ContextSourceFrags:     base,
				ContextToolUsage:       usage,
				ContextBudgetMaxTokens: window,
			}
			if mode == sessionmode.Discuss {
				cfg.ContextSourceFrags = nil
				cfg.System = agentpkg.GenerateSystemPrompt(agentpkg.SystemPromptParams{
					SessionType: mode,
					Timezone:    "UTC",
				}) + "\n\n" + usage
			}
			out, err := ApplyProviderRunConfig(context.Background(), nil, cfg)
			if err != nil {
				t.Fatalf("ApplyProviderRunConfig() error = %v", err)
			}

			if mode == sessionmode.Discuss {
				if out.ContextManifest.BudgetPlan != nil {
					t.Fatalf("discuss budget plan = %#v, want disabled", out.ContextManifest.BudgetPlan)
				}
				if !hasFragID(out.ContextFrags, "system.tool_usage") ||
					hasFragID(out.ContextFrags, systemBudgetMarkerID) {
					t.Fatalf("discuss selected IDs = %v, want unpruned tool usage and no marker", fragIDs(out.ContextFrags))
				}
				records := out.ContextManifest.Mutations.Records()
				if len(records) != 1 ||
					records[0].Kind != contextfrag.MutationContextBudgetDisabled ||
					records[0].Detail != "discuss_flat_reverse_parse" {
					t.Fatalf("discuss mutations = %#v, want visible plan-disabled exception", records)
				}
				return
			}

			plan := out.ContextManifest.BudgetPlan
			if plan == nil || plan.ActualSystemCost > plan.SystemBudget {
				t.Fatalf("active mode budget plan = %#v", plan)
			}
			usageDecision, ok := decisionByID(out.ContextManifest.SelectionDecisions, "system.tool_usage")
			if !ok ||
				usageDecision.Decision != contextfrag.DecisionDropped ||
				usageDecision.Reason != systemBudgetDropReason {
				t.Fatalf("tool usage decision = %#v, %v; want dropped/system_budget", usageDecision, ok)
			}
			if !hasFragID(out.ContextFrags, systemBudgetMarkerID) ||
				!strings.Contains(out.System, "[System Notice]") {
				t.Fatalf("selected IDs/system = %v/%q, want explicit omission marker", fragIDs(out.ContextFrags), out.System)
			}
			for _, id := range []string{"system.prompt.intro", "system.prompt.body", "system.prompt.tail"} {
				decision, found := decisionByID(out.ContextManifest.SelectionDecisions, id)
				if !found || decision.Decision != contextfrag.DecisionSelected {
					t.Fatalf("required section %s decision = %#v, %v", id, decision, found)
				}
			}
		})
	}
}

func TestNativeModeProtectedOverflowFailsClosedExceptDiscuss(t *testing.T) {
	t.Parallel()

	window := contextWindowForDefaultOutputReserve(MinimumSystemBudgetTokens)
	for _, mode := range round6NativeModes {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			out, err := ApplyProviderRunConfig(context.Background(), nil, agentpkg.RunConfig{
				SessionType:            mode,
				System:                 round6FlatSystemPrompt(mode),
				ContextSourceFrags:     round6ProtectedOverflowSourceFrags(mode),
				ContextBudgetMaxTokens: window,
			})

			if mode == sessionmode.Discuss {
				if err != nil || out.ContextManifest.BudgetPlan != nil {
					t.Fatalf("discuss error/plan = %v/%#v, want documented disabled exception", err, out.ContextManifest.BudgetPlan)
				}
				records := out.ContextManifest.Mutations.Records()
				if len(records) != 1 ||
					records[0].Kind != contextfrag.MutationContextBudgetDisabled {
					t.Fatalf("discuss mutations = %#v, want plan-disabled exception", records)
				}
				return
			}

			if !errors.Is(err, contextfrag.ErrProtectedContextOverflow) {
				t.Fatalf("ApplyProviderRunConfig() error = %v, want ErrProtectedContextOverflow", err)
			}
			records := out.ContextManifest.Mutations.Records()
			if len(records) != 1 ||
				records[0].Kind != contextfrag.MutationContextBudgetFailure ||
				records[0].Detail != "protected_context_overflow" {
				t.Fatalf("budget failure mutations = %#v", records)
			}
			for _, record := range records {
				if record.Kind == contextfrag.MutationContextViewFallback {
					t.Fatalf("protected overflow triggered legacy fallback: %#v", records)
				}
			}
		})
	}
}

func TestOversizedDynamicSystemSourcesPruneWithExplicitMarker(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		build func(*testing.T) ([]contextfrag.ContextFrag, []string, string)
	}{
		{
			name: "skills catalog",
			build: func(t *testing.T) ([]contextfrag.ContextFrag, []string, string) {
				t.Helper()
				params := agentpkg.SystemPromptParams{
					SessionType: sessionmode.Chat,
					Timezone:    "UTC",
					Skills: []agentpkg.SkillEntry{
						{Name: "alpha", Description: strings.Repeat("猫😺", 2000)},
						{Name: "技能", Description: strings.Repeat("界🌏", 2000)},
					},
				}
				return agentpkg.SystemSectionFrags(
						agentpkg.GenerateSystemSections(params),
						contextfrag.Scope{},
					),
					[]string{"system.skill.alpha", "system.skill.技能", "system.skills.header"},
					""
			},
		},
		{
			name: "platform identities",
			build: func(t *testing.T) ([]contextfrag.ContextFrag, []string, string) {
				t.Helper()
				items := []agentpkg.SystemPromptItem{
					{
						ID:   "telegram-large",
						Text: `<identity channel="telegram" username="` + strings.Repeat("猫😺", 2000) + `"/>`,
					},
					{
						ID:   "微信-海量",
						Text: `<identity channel="wechat" username="` + strings.Repeat("界🌏", 2000) + `"/>`,
					},
				}
				params := agentpkg.SystemPromptParams{
					SessionType: sessionmode.Chat,
					Timezone:    "UTC",
					PlatformIdentitiesSection: "## Platform Identities\n\n" +
						items[0].Text + "\n" + items[1].Text,
					PlatformIdentities: items,
				}
				return agentpkg.SystemSectionFrags(
						agentpkg.GenerateSystemSections(params),
						contextfrag.Scope{},
					),
					[]string{
						"system.platform_identity.telegram-large",
						"system.platform_identity.微信-海量",
						"system.platform_identity.header",
					},
					""
			},
		},
		{
			name: "workspace file",
			build: func(t *testing.T) ([]contextfrag.ContextFrag, []string, string) {
				t.Helper()
				params := agentpkg.SystemPromptParams{
					SessionType:   sessionmode.Chat,
					Timezone:      "UTC",
					MaxFilesBytes: 1024,
					Files: []agentpkg.SystemFile{{
						Filename: "AGENTS.md",
						Content:  strings.Repeat("规则猫😺\n", 1000),
					}},
				}
				frags := agentpkg.SystemSectionFrags(
					agentpkg.GenerateSystemSections(params),
					contextfrag.Scope{},
				)
				id := "system.workspace_file.AGENTS.md"
				workspace := fragByID(frags, id)
				if workspace == nil ||
					!utf8.ValidString(workspace.Parts[0].Text) ||
					!strings.Contains(workspace.Parts[0].Text, "[memoh pruned]") {
					t.Fatalf("locally pruned workspace fragment = %#v", workspace)
				}
				return frags, []string{id}, ""
			},
		},
		{
			name: "hook section",
			build: func(t *testing.T) ([]contextfrag.ContextFrag, []string, string) {
				t.Helper()
				frags := round6StaticSystemFrags(sessionmode.Chat, contextfrag.Scope{})
				id := "system.hook.round6.动态"
				hook := hookSystemTestFrag(
					id,
					strings.Repeat("猫😺", 5000),
					contextfrag.RetentionOptional,
					contextfrag.CacheDynamic,
					contextfrag.TrustWorkspace,
					80,
					contextfrag.Scope{},
				)
				hook.Budget = contextfrag.BudgetPolicy{
					MaxTokens: 64,
					Overflow:  contextfrag.OverflowTrim,
				}
				return append(frags, hook), []string{id}, id
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			frags, droppedIDs, editedID := tt.build(t)
			base := withoutFragIDs(frags, droppedIDs)
			marker := systemBudgetMarkerFrag(droppedIDs, contextfrag.Scope{})
			const toolDefsCost = 1
			window := contextWindowForDefaultOutputReserve(
				systemFragCost(appendClone(base, marker)) + toolDefsCost,
			)

			out, err := ApplyProviderRunConfig(context.Background(), nil, agentpkg.RunConfig{
				SessionType:             sessionmode.Chat,
				ContextSourceFrags:      frags,
				ContextBudgetMaxTokens:  window,
				ContextToolDefsResolved: true,
				ContextToolDefs: []contextfrag.ToolDefAccounting{{
					Provider:      "native",
					Name:          "use_skill",
					TokenEstimate: toolDefsCost,
				}},
			})
			if err != nil {
				t.Fatalf("ApplyProviderRunConfig() error = %v", err)
			}
			for _, id := range droppedIDs {
				decision, ok := decisionByID(out.ContextManifest.SelectionDecisions, id)
				if !ok ||
					decision.Decision != contextfrag.DecisionDropped ||
					decision.Reason != systemBudgetDropReason {
					t.Fatalf("decision for %s = %#v, %v; want dropped/system_budget", id, decision, ok)
				}
			}
			markerFrag := fragByID(out.ContextFrags, systemBudgetMarkerID)
			markerItem := manifestItemByID(out.ContextManifest.Items, systemBudgetMarkerID)
			if markerFrag == nil || markerItem == nil ||
				!utf8.ValidString(markerFrag.Parts[0].Text) ||
				!strings.Contains(markerFrag.Parts[0].Text, "[System Notice]") ||
				markerItem.TokenEstimate != contextfrag.ResolveFragTokens(*markerFrag) {
				t.Fatalf("marker frag/item = %#v/%#v", markerFrag, markerItem)
			}
			plan := out.ContextManifest.BudgetPlan
			if plan == nil || plan.ActualSystemCost > plan.SystemBudget {
				t.Fatalf("budget plan = %#v", plan)
			}
			if editedID != "" {
				if !hasEditTrace(out.ContextManifest.EditTrace, "frag_budget.trim."+editedID, contextfrag.EditReplace) ||
					!hasEditTrace(out.ContextManifest.EditTrace, "selection.drop."+editedID, contextfrag.EditRemove) {
					t.Fatalf("hook edit trace = %#v, want trim then drop audit", out.ContextManifest.EditTrace)
				}
			}
		})
	}
}

func TestFragBudgetMaxTokensTrimsUnicodeWithMarkerAndEstimate(t *testing.T) {
	t.Parallel()

	source := hookSystemTestFrag(
		"unicode.max_tokens",
		strings.Repeat("猫😺", 80),
		contextfrag.RetentionOptional,
		contextfrag.CacheDynamic,
		contextfrag.TrustWorkspace,
		80,
		contextfrag.Scope{},
	)
	source.Budget = contextfrag.BudgetPolicy{MaxTokens: 32, Overflow: contextfrag.OverflowTrim}
	source = contextfrag.NormalizeContextRefs([]contextfrag.ContextFrag{source})[0]

	selector := &FragmentSelector{}
	result := selector.Select(
		[]contextfrag.ContextFrag{source},
		selector.ProfileFor(contextfrag.IntentRunConfigPreProvider),
		BudgetEnvelope{},
	)
	if result.FatalError != nil || len(result.Selected) != 1 {
		t.Fatalf("Select() error/selected = %v/%v", result.FatalError, fragIDs(result.Selected))
	}
	text := result.Selected[0].Parts[0].Text
	if !utf8.ValidString(text) ||
		!strings.Contains(text, "[trimmed from ") ||
		len(text) > source.Budget.MaxTokens*fragBudgetTokenByteFactor ||
		contextfrag.ResolveFragTokens(result.Selected[0]) > source.Budget.MaxTokens {
		t.Fatalf("trimmed Unicode text/estimate = %q/%d", text, contextfrag.ResolveFragTokens(result.Selected[0]))
	}
	decisions := selectionDecisions([]contextfrag.ContextFrag{source}, result)
	if len(decisions) != 1 ||
		decisions[0].Decision != contextfrag.DecisionTrimmed ||
		decisions[0].Reason != "frag_budget:max_tokens" {
		t.Fatalf("selection decisions = %#v", decisions)
	}
}

func round6StaticSystemFrags(mode string, scope contextfrag.Scope) []contextfrag.ContextFrag {
	return agentpkg.SystemSectionFrags(agentpkg.GenerateSystemSections(agentpkg.SystemPromptParams{
		SessionType: mode,
		Timezone:    "UTC",
	}), scope)
}

func round6FlatSystemPrompt(mode string) string {
	if mode != sessionmode.Discuss {
		return ""
	}
	return agentpkg.GenerateSystemPrompt(agentpkg.SystemPromptParams{
		SessionType: mode,
		Timezone:    "UTC",
	})
}

func round6ProtectedOverflowSourceFrags(mode string) []contextfrag.ContextFrag {
	if mode == sessionmode.Discuss {
		return nil
	}
	return round6StaticSystemFrags(mode, contextfrag.Scope{})
}

func appendClone(frags []contextfrag.ContextFrag, extra contextfrag.ContextFrag) []contextfrag.ContextFrag {
	out := append([]contextfrag.ContextFrag(nil), frags...)
	return append(out, extra)
}

func withoutFragIDs(frags []contextfrag.ContextFrag, ids []string) []contextfrag.ContextFrag {
	dropped := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		dropped[id] = struct{}{}
	}
	out := make([]contextfrag.ContextFrag, 0, len(frags))
	for _, frag := range frags {
		if _, ok := dropped[frag.ID]; !ok {
			out = append(out, frag)
		}
	}
	return out
}

func fragByID(frags []contextfrag.ContextFrag, id string) *contextfrag.ContextFrag {
	for i := range frags {
		if frags[i].ID == id {
			return &frags[i]
		}
	}
	return nil
}

func hasEditTrace(trace []contextfrag.ContextEditTrace, id string, op contextfrag.ContextEditOp) bool {
	for _, edit := range trace {
		if edit.EditID == id && edit.Op == op {
			return true
		}
	}
	return false
}
