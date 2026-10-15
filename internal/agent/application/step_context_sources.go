package application

import (
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

func committedContextSources(count int, origins []int, persisted []messagepkg.Message) []contextfrag.ContextRef {
	sources := make([]contextfrag.ContextRef, count)
	if len(origins) != len(persisted) {
		return sources
	}
	seen := make(map[int]bool, count)
	for i, origin := range origins {
		if origin < 0 || origin >= count {
			continue
		}
		if seen[origin] {
			sources[origin] = contextfrag.ContextRef{}
			continue
		}
		seen[origin] = true
		if record, err := historyfrag.FromDBMessage(persisted[i], historyfrag.ScopeFallback{}); err == nil {
			sources[origin] = record.Ref
		}
	}
	return sources
}
