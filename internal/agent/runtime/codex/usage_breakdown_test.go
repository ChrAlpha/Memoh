package codex

import (
	"log/slog"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/runtime/codex/protocol"
	"github.com/felinics/memoh/internal/agent/runtime/external"
)

func TestTurnUsageCarriesTheInputBreakdownAcrossRequests(t *testing.T) {
	turn := newTurnState(t.Context(), external.PromptInput{Sink: &captureSink{}}, "thread", nil, nil, nil, nil, slog.Default())
	defer turn.close()
	turn.setTurnID("current")
	write := int64(60)
	for _, last := range []protocol.TokenUsageBreakdown{
		{InputTokens: 100, CachedInputTokens: 40, OutputTokens: 5, ReasoningOutputTokens: 2, TotalTokens: 105},
		{InputTokens: 200, CachedInputTokens: 120, CacheWriteInputTokens: &write, OutputTokens: 7, ReasoningOutputTokens: 3, TotalTokens: 207},
	} {
		turn.handleNotification(&protocol.ThreadTokenUsageUpdatedNotification{ThreadID: "thread", TurnID: "current", TokenUsage: protocol.ThreadTokenUsage{Last: last}})
	}
	turn.handleNotification(&protocol.TurnCompletedNotification{ThreadID: "thread", Turn: protocol.Turn{ID: "current", Status: protocol.TurnStatusCompleted}})
	result, err := turn.result()
	if err != nil {
		t.Fatal(err)
	}
	want := sdk.Usage{
		InputTokens: 300, OutputTokens: 12, TotalTokens: 312, ReasoningTokens: 5, CachedInputTokens: 160,
		InputTokenDetails:  sdk.InputTokenDetail{NoCacheTokens: 80, CacheReadTokens: 160, CacheWriteTokens: 60},
		OutputTokenDetails: sdk.OutputTokenDetail{ReasoningTokens: 5},
	}
	if result.Usage == nil || *result.Usage != want {
		t.Fatalf("usage = %+v, want %+v", result.Usage, want)
	}
}
