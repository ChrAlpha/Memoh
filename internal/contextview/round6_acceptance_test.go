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

func TestNativeModeSystemBudgetPressure(t *testing.T) {
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
			out, err := ProviderRunConfigApplier(nil)(context.Background(), cfg)
			if err != nil {
				t.Fatalf("ApplyProviderRunConfig() error = %v", err)
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

func TestNativeModeProtectedOverflowFailsClosed(t *testing.T) {
	t.Parallel()

	window := contextWindowForDefaultOutputReserve(MinimumSystemBudgetTokens)
	for _, mode := range round6NativeModes {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			out, err := ProviderRunConfigApplier(nil)(context.Background(), agentpkg.RunConfig{
				SessionType:            mode,
				ContextSourceFrags:     round6ProtectedOverflowSourceFrags(mode),
				ContextBudgetMaxTokens: window,
			})

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

func TestProviderUsesByteEstimatorForStaticSystemFragsWithoutTokenizer(t *testing.T) {
	t.Parallel()

	for _, mode := range round6NativeModes {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			params := agentpkg.SystemPromptParams{SessionType: mode, Timezone: "UTC"}
			frags := agentpkg.SystemSectionFrags(
				agentpkg.GenerateSystemSections(params),
				contextfrag.Scope{},
			)
			resolvedCost := 0
			expectedTokens := make(map[string]int, len(frags))
			for _, frag := range frags {
				if frag.TokenEstimate != 0 {
					t.Fatalf("static fragment %s has preset token estimate %d", frag.ID, frag.TokenEstimate)
				}
				textBytes := 0
				for _, part := range frag.Parts {
					if part.Type != contextfrag.PartText {
						t.Fatalf("static fragment %s has non-text part %s", frag.ID, part.Type)
					}
					textBytes += len(part.Text)
				}
				expectedTokens[frag.ID] = contextfrag.TokensFromBytes(textBytes)
				resolvedCost += expectedTokens[frag.ID]
			}
			if len(frags) > 1 {
				resolvedCost += len(frags) - 1
			}
			renderedPrompt := agentpkg.GenerateSystemPrompt(params)
			renderedCost := ((len(renderedPrompt) + contextfrag.EstimateBytesPerToken - 1) /
				contextfrag.EstimateBytesPerToken) * contextfrag.ProviderBudgetSafetyFactorPercent / 100
			wantCost := max(resolvedCost, renderedCost)

			out, err := ProviderRunConfigApplier(nil)(context.Background(), agentpkg.RunConfig{
				SessionType:            mode,
				ContextSourceFrags:     frags,
				ContextBudgetMaxTokens: contextWindowForDefaultOutputReserve(wantCost + 128),
			})
			if err != nil {
				t.Fatalf("ApplyProviderRunConfig() error = %v", err)
			}
			if out.System != renderedPrompt {
				t.Fatalf("provider system prompt changed without pressure")
			}
			if plan := out.ContextManifest.BudgetPlan; plan == nil || plan.ActualSystemCost != wantCost {
				t.Fatalf("provider budget plan = %#v, want byte-estimated system cost %d", plan, wantCost)
			}
			for _, frag := range frags {
				item := manifestItemByID(out.ContextManifest.Items, frag.ID)
				if item == nil {
					t.Fatalf("manifest missing static fragment %s", frag.ID)
				}
				if want := expectedTokens[frag.ID]; item.TokenEstimate != want {
					t.Fatalf(
						"manifest token estimate for %s = %d, want byte estimate %d",
						frag.ID,
						item.TokenEstimate,
						want,
					)
				}
			}
		})
	}
}

func TestOversizedHookSystemSectionPrunesWithExplicitMarker(t *testing.T) {
	t.Parallel()

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
	frags = append(frags, hook)
	base := make([]contextfrag.ContextFrag, 0, len(frags)-1)
	for _, frag := range frags {
		if frag.ID != id {
			base = append(base, frag)
		}
	}
	marker := systemBudgetMarkerFrag([]string{id}, contextfrag.Scope{})
	const toolDefsCost = 1
	window := contextWindowForDefaultOutputReserve(
		systemFragCost(appendClone(base, marker)) + toolDefsCost,
	)

	out, err := applyProviderRunConfig(context.Background(), nil, agentpkg.RunConfig{
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
		t.Fatalf("applyProviderRunConfig() error = %v", err)
	}
	decision, ok := decisionByID(out.ContextManifest.SelectionDecisions, id)
	if !ok ||
		decision.Decision != contextfrag.DecisionDropped ||
		decision.Reason != systemBudgetDropReason {
		t.Fatalf("decision for %s = %#v, %v; want dropped/system_budget", id, decision, ok)
	}
	markerFrag := round6FragByID(out.ContextFrags, systemBudgetMarkerID)
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
	if !round6HasEditTrace(out.ContextManifest.EditTrace, "frag_budget.trim."+id, contextfrag.EditReplace) ||
		!round6HasEditTrace(out.ContextManifest.EditTrace, "selection.drop."+id, contextfrag.EditRemove) {
		t.Fatalf("hook edit trace = %#v, want trim then drop audit", out.ContextManifest.EditTrace)
	}
}

func round6StaticSystemFrags(mode string, scope contextfrag.Scope) []contextfrag.ContextFrag {
	return agentpkg.SystemSectionFrags(agentpkg.GenerateSystemSections(agentpkg.SystemPromptParams{
		SessionType: mode,
		Timezone:    "UTC",
	}), scope)
}

func round6ProtectedOverflowSourceFrags(mode string) []contextfrag.ContextFrag {
	return round6StaticSystemFrags(mode, contextfrag.Scope{})
}

func appendClone(frags []contextfrag.ContextFrag, extra contextfrag.ContextFrag) []contextfrag.ContextFrag {
	out := append([]contextfrag.ContextFrag(nil), frags...)
	return append(out, extra)
}

func manifestItemByID(items []contextfrag.ManifestItem, id string) *contextfrag.ManifestItem {
	for i := range items {
		if items[i].ID == id {
			return &items[i]
		}
	}
	return nil
}

func round6FragByID(frags []contextfrag.ContextFrag, id string) *contextfrag.ContextFrag {
	for i := range frags {
		if frags[i].ID == id {
			return &frags[i]
		}
	}
	return nil
}

func round6HasEditTrace(
	trace []contextfrag.ContextEditTrace,
	id string,
	op contextfrag.ContextEditOp,
) bool {
	for _, edit := range trace {
		if edit.EditID == id && edit.Op == op {
			return true
		}
	}
	return false
}
