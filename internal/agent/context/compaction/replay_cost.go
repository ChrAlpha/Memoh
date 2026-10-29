package compaction

import (
	sdk "github.com/felinics/twilight/sdk"
	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
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
		return contextfrag.ProviderBudgetTokensFromBytes(len(message.RawContent)) + runtimeContextReplayTokens(message.Record)
	}
	return replayTokens(sdkMessage) + runtimeContextReplayTokens(message.Record)
}

// runtimeContextReplayTokens is what the runtime context a request row
// recorded costs where history replays it, right after the row.
func runtimeContextReplayTokens(record historyfrag.HistoryRecord) int {
	tokens := 0
	for _, item := range historyfrag.RuntimeContextFromMetadata(record.DBMessageID, record.Metadata) {
		tokens += replayTokens(messageconv.ModelMessageToSDKMessage(item.Message))
	}
	return tokens
}

func replayTokens(message sdk.Message) int {
	return contextfrag.ResolveProviderBudgetFragTokens(contextfrag.MessageFrag(contextfrag.MessageFragInput{Message: message}))
}
