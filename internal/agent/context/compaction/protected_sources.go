package compaction

import "github.com/felinics/memoh/internal/agent/turn"

func isProtectedSource(candidate CompactionCandidate, sources []turn.ContextMessageSource) bool {
	for _, source := range sources {
		if source.ID == "" {
			continue
		}
		if source.Kind == "external" && source.ID == candidate.Record.ExternalMessageID {
			return true
		}
		if source.Kind == "history" && source.ID == candidate.ID.String() {
			return true
		}
	}
	return false
}

func protectCurrentSources(messages []CompactionCandidate, sources []turn.ContextMessageSource) {
	for i := range messages {
		if isProtectedSource(messages[i], sources) {
			for j := i; j < len(messages); j++ {
				messages[j].Policies = appendPolicy(messages[j].Policies, CompactPolicyPreserveRecent)
				messages[j].Policies = appendPolicy(messages[j].Policies, CompactPolicyMustKeep)
			}
			propagateMustKeepAcrossToolExchanges(messages)
			return
		}
	}
}

func protectFrom(messages []CompactionCandidate, from int) {
	for j := from; j < len(messages); j++ {
		messages[j].Policies = appendPolicy(messages[j].Policies, CompactPolicyPreserveRecent)
		messages[j].Policies = appendPolicy(messages[j].Policies, CompactPolicyMustKeep)
	}
	propagateMustKeepAcrossToolExchanges(messages)
}

func protectedSourceIDs(sources []turn.ContextMessageSource) (history, external []string) {
	for _, source := range sources {
		switch {
		case source.ID == "":
		case source.Kind == "history":
			history = append(history, source.ID)
		case source.Kind == "external":
			external = append(external, source.ID)
		}
	}
	return history, external
}
