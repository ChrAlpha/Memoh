package application

import (
	"fmt"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/hooks"
)

const (
	hookSystemSectionPriority  = 80
	maxHookSystemSectionChars  = 8 * 1024
	hookSystemSectionSource    = "hook_system_section"
	hookSystemSectionCollector = "hook_system_sections"
)

type promptHookOutput struct {
	Event  string
	Result hooks.Result
}

type hookSystemSectionBuild struct {
	Frags    []contextfrag.ContextFrag
	Warnings []contextfrag.ValidationWarning
}

type hookSystemSectionWarning struct {
	code      string
	message   string
	fragIndex int
}

func buildHookSystemSections(outputs []promptHookOutput, scope contextfrag.Scope) hookSystemSectionBuild {
	usedIDs := make(map[string]struct{})
	var frags []contextfrag.ContextFrag
	var attachedWarnings []hookSystemSectionWarning
	var outputWarnings []contextfrag.ValidationWarning
	for _, output := range outputs {
		for _, section := range output.Result.AppendSystemSections {
			id := uniqueHookSystemSectionID(section, usedIDs)
			frag := contextfrag.TextFrag(contextfrag.TextFragInput{
				ID:            id,
				Kind:          contextfrag.KindHookContext,
				Role:          sdk.MessageRoleSystem,
				Slot:          contextfrag.SlotSystem,
				Text:          section.Text,
				Priority:      hookSystemSectionPriority,
				RetentionTier: hookSystemSectionRetention(section.Retention),
				CacheClass:    hookSystemSectionCache(section.Cache),
				Trust:         contextfrag.TrustWorkspace,
				Scope:         scope,
				Source:        hookSystemSectionSource,
				SourceID:      hookSystemSectionSourceID(output.Event, section),
				Collector:     hookSystemSectionCollector,
				Index:         len(frags),
				Render:        contextfrag.RenderPolicy{Format: contextfrag.RenderMarkdown},
				Budget: contextfrag.BudgetPolicy{
					MaxChars: maxHookSystemSectionChars,
					Overflow: contextfrag.OverflowTrim,
				},
			})
			fragIndex := len(frags)
			frags = append(frags, frag)
			for _, code := range section.WarningCodes {
				attachedWarnings = append(attachedWarnings, hookSystemSectionWarning{
					code:      code,
					message:   hookSystemSectionWarningMessage(code),
					fragIndex: fragIndex,
				})
			}
		}
		for _, warning := range output.Result.Warnings {
			if warning.Code == hooks.WarningSystemSectionRequiredClamped {
				continue
			}
			outputWarnings = append(outputWarnings, contextfrag.ValidationWarning{
				Code:    warning.Code,
				Message: warning.Message,
			})
		}
	}

	frags = contextfrag.NormalizeContextRefs(frags)
	warnings := make([]contextfrag.ValidationWarning, 0, len(attachedWarnings)+len(outputWarnings))
	for _, warning := range attachedWarnings {
		warnings = append(warnings, contextfrag.ValidationWarning{
			Code:    warning.code,
			Message: warning.message,
			Ref:     frags[warning.fragIndex].Ref,
		})
	}
	warnings = append(warnings, outputWarnings...)
	return hookSystemSectionBuild{Frags: frags, Warnings: warnings}
}

func uniqueHookSystemSectionID(section hooks.SystemSectionOutput, used map[string]struct{}) string {
	hookName := strings.TrimSpace(section.HookName)
	if hookName == "" {
		hookName = "unnamed"
	}
	base := "system.hook." + hookName
	if declaredID := strings.TrimSpace(section.ID); declaredID != "" {
		base += "." + declaredID
	}
	id := base
	for suffix := 2; ; suffix++ {
		if _, exists := used[id]; !exists {
			used[id] = struct{}{}
			return id
		}
		id = fmt.Sprintf("%s.%d", base, suffix)
	}
}

func hookSystemSectionRetention(retention hooks.SystemSectionRetention) contextfrag.RetentionTier {
	if retention == hooks.SystemSectionRetentionPreferred {
		return contextfrag.RetentionPreferred
	}
	return contextfrag.RetentionOptional
}

func hookSystemSectionCache(cache hooks.SystemSectionCache) contextfrag.CacheClass {
	if cache == hooks.SystemSectionCacheStable {
		return contextfrag.CacheStable
	}
	return contextfrag.CacheDynamic
}

func hookSystemSectionSourceID(event string, section hooks.SystemSectionOutput) string {
	return strings.TrimSpace(event) + ":" + strings.TrimSpace(section.HookName)
}

func hookSystemSectionWarningMessage(code string) string {
	if code == hooks.WarningSystemSectionRequiredClamped {
		return "hook system section retention was clamped from required to preferred"
	}
	return ""
}

func hookSystemSectionTexts(result hooks.Result) []string {
	texts := make([]string, 0, len(result.AppendSystemSections))
	for _, section := range result.AppendSystemSections {
		if text := strings.TrimSpace(section.Text); text != "" {
			texts = append(texts, text)
		}
	}
	return texts
}
