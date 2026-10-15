package application

import (
	"sort"
	"strings"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

func projectContextSwitch(input native.ContextSwitchInput, artifacts []compaction.Artifact) native.ContextSwitchResult {
	if len(input.Sources) != len(input.Request.Messages) || len(input.Protected) != len(input.Sources) {
		return native.ContextSwitchResult{}
	}
	type replacement struct {
		start, end int
		artifact   compaction.Artifact
	}
	var replacements []replacement
	for _, artifact := range artifacts {
		if artifact.ID == "" || strings.TrimSpace(artifact.Summary) == "" || len(artifact.Coverage) == 0 || artifact.CoverageMalformed {
			continue
		}
		covered := make(map[string]contextfrag.ContextRef, len(artifact.Coverage))
		for _, source := range artifact.Coverage {
			covered[source.Ref.StableKey()] = source.Ref
		}
		if len(covered) != len(artifact.Coverage) {
			continue
		}
		seen := make(map[string]bool, len(covered))
		start, end, count := -1, -1, 0
		valid := true
		for i, source := range input.Sources {
			refs := contextSwitchSourceCoverage(source)
			matched := 0
			for _, ref := range refs {
				if other, ok := covered[ref.StableKey()]; ok && other == ref {
					matched++
				}
			}
			if matched == 0 {
				continue
			}
			if matched != len(refs) || input.Protected[i] {
				valid = false
				break
			}
			for _, ref := range refs {
				key := ref.StableKey()
				if seen[key] {
					valid = false
				}
				seen[key] = true
			}
			if start < 0 {
				start = i
			}
			end = i + 1
			count++
		}
		if valid && start >= 0 && count == end-start && len(seen) == len(covered) {
			replacements = append(replacements, replacement{start: start, end: end, artifact: artifact})
		}
	}
	if len(replacements) == 0 {
		return native.ContextSwitchResult{}
	}
	sort.Slice(replacements, func(i, j int) bool { return replacements[i].start < replacements[j].start })
	for i := 1; i < len(replacements); i++ {
		if replacements[i].start < replacements[i-1].end {
			return native.ContextSwitchResult{}
		}
	}
	var result native.ContextSwitchResult
	next := 0
	for i := 0; i < len(input.Request.Messages); {
		if next < len(replacements) && replacements[next].start == i {
			replacement := replacements[next]
			record := replacement.artifact.HistoryRecord(contextfrag.Scope{})
			result.Messages = append(result.Messages, modelMessageToSDKMessage(record.ModelMessage))
			result.SourceIndexes = append(result.SourceIndexes, -1)
			coverage := make([]contextfrag.ContextRef, 0, len(replacement.artifact.Coverage))
			for _, source := range replacement.artifact.Coverage {
				coverage = append(coverage, source.Ref)
			}
			result.Sources = append(result.Sources, native.ContextSwitchSource{Ref: record.Ref, Coverage: coverage})
			i = replacement.end
			next++
			continue
		}
		result.Messages = append(result.Messages, input.Request.Messages[i])
		result.SourceIndexes = append(result.SourceIndexes, i)
		result.Sources = append(result.Sources, input.Sources[i])
		i++
	}
	return result
}

func contextSwitchSourceCoverage(source native.ContextSwitchSource) []contextfrag.ContextRef {
	if len(source.Coverage) > 0 {
		return source.Coverage
	}
	if source.Ref.ID == "" || source.Ref.ContentHash == "" || source.Ref.Durability != contextfrag.RefDurable {
		return nil
	}
	return []contextfrag.ContextRef{source.Ref}
}
