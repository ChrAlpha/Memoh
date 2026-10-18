package compaction

import (
	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/messageconv"
)

// compactedReplayTokens is the provider replay cost of the rows a summary would
// replace, counting rows that render to no prompt text.
func compactedReplayTokens(messages []CompactionCandidate, compacted []pgtype.UUID) int {
	selected := make(map[pgtype.UUID]bool, len(compacted))
	for _, id := range compacted {
		selected[id] = true
	}
	tokens := 0
	for _, message := range messages {
		if selected[message.ID] {
			tokens += messageReplayTokens(message)
		}
	}
	return tokens
}

func messageReplayTokens(message CompactionCandidate) int {
	sdkMessage := messageconv.ModelMessageToSDKMessage(message.Record.ModelMessage)
	if len(sdkMessage.Content) == 0 {
		return contextfrag.ProviderBudgetTokensFromBytes(len(message.RawContent))
	}
	return contextfrag.ResolveProviderBudgetFragTokens(contextfrag.MessageFrag(contextfrag.MessageFragInput{Message: sdkMessage}))
}
