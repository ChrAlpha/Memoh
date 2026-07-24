package native

import (
	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/models"
)

// RefreshContextFrag rebuilds the typed context frag view from the legacy
// RunConfig fields. The SDK-facing fields remain the source of truth in phase 1.
func (cfg RunConfig) RefreshContextFrag() RunConfig {
	query := cfg.Query
	inlineImages := cfg.InlineImages
	if cfg.ContextQueryMaterialized {
		query = ""
		inlineImages = nil
	}
	assembled := contextfrag.Compile(contextfrag.CompileInput{
		Source:                  contextfrag.SourceRunConfig,
		Scope:                   cfg.ContextScope,
		System:                  cfg.System,
		Messages:                cfg.Messages,
		CurrentUserMessageIndex: cfg.ContextCurrentUserMessageIndex,
		Query:                   query,
		InlineImages:            inlineImages,
		ToolUsage:               cfg.ContextToolUsage,
		DynamicMutators:         cfg.ContextDynamicMutators,
		Existing:                cfg.ContextFrags,
	})
	cfg.ContextFrags = assembled.Frags
	cfg.ContextManifest = preserveLifecycleAccounting(cfg.ContextManifest, assembled.Manifest)
	if cfg.ContextLifecycle != nil {
		cfg.ContextLifecycle.SetManifest(cfg.ContextManifest)
	}
	return cfg
}

func preserveLifecycleAccounting(previous, next contextfrag.Manifest) contextfrag.Manifest {
	if previous.CachePlan != nil && next.CachePlan == nil {
		plan := *previous.CachePlan
		next.CachePlan = &plan
	}
	if previous.Mutations != nil && next.Mutations == nil {
		next.Mutations = previous.Mutations
	}
	if previous.Selection != nil && next.Selection == nil {
		selection := *previous.Selection
		if len(previous.Selection.DropReasons) > 0 {
			selection.DropReasons = make(map[string]int, len(previous.Selection.DropReasons))
			for reason, count := range previous.Selection.DropReasons {
				selection.DropReasons[reason] = count
			}
		}
		next.Selection = &selection
	}
	return next
}

func (cfg RunConfig) contextDynamicMutators(readMedia bool, beforeModelCallHook bool, injectCh bool) []contextfrag.DynamicMutator {
	var mutators []contextfrag.DynamicMutator
	if cfg.Model != nil &&
		models.ResolveClientType(cfg.Model) == string(models.ClientTypeAnthropicMessages) &&
		models.NormalizePromptCacheTTL(cfg.PromptCacheTTL) != models.PromptCacheTTLOff {
		mutators = append(mutators, contextfrag.DynamicMutatorPromptCache)
	}
	if injectCh && cfg.InjectCh != nil {
		mutators = append(mutators, contextfrag.DynamicMutatorInjectCh)
	}
	if readMedia {
		mutators = append(mutators, contextfrag.DynamicMutatorReadMedia)
	}
	if beforeModelCallHook {
		mutators = append(mutators, contextfrag.DynamicMutatorBeforeModelCallHook)
	}
	if cfg.BackgroundManager != nil {
		mutators = append(mutators, contextfrag.DynamicMutatorBackgroundSummary)
	}
	mutators = append(mutators, contextfrag.DynamicMutatorMidTaskPrune)
	return mutators
}
