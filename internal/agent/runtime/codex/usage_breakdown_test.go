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
	write, total := int64(60), int64(0)
	for _, last := range []protocol.TokenUsageBreakdown{
		{InputTokens: 100, CachedInputTokens: 40, OutputTokens: 5, ReasoningOutputTokens: 2, TotalTokens: 105},
		{InputTokens: 200, CachedInputTokens: 120, CacheWriteInputTokens: &write, OutputTokens: 7, ReasoningOutputTokens: 3, TotalTokens: 207},
	} {
		total += last.TotalTokens
		turn.handleNotification(&protocol.ThreadTokenUsageUpdatedNotification{ThreadID: "thread", TurnID: "current", TokenUsage: protocol.ThreadTokenUsage{Last: last, Total: protocol.TokenUsageBreakdown{TotalTokens: total}}})
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

// Codex sends the thread's usage again unchanged when only rate limits move,
// and sends a last with no input or output after recomputing the context or
// filling it on overflow. Only requests add to the turn; the thread total is
// not monotonic (a fork restarts it lower), so it is not the test.
func TestTurnUsageCountsOnlyRequests(t *testing.T) {
	previous := protocol.ThreadTokenUsage{
		Last:  protocol.TokenUsageBreakdown{InputTokens: 100, CachedInputTokens: 80, OutputTokens: 5, TotalTokens: 105},
		Total: protocol.TokenUsageBreakdown{TotalTokens: 5000},
	}
	srv := &appServer{turns: map[string]*turnState{}, threadUsage: map[string]protocol.ThreadTokenUsage{"thread": previous}}
	turn := newTurnState(t.Context(), external.PromptInput{Sink: &captureSink{}}, "thread", nil, nil, nil, nil, slog.Default())
	defer turn.close()
	srv.registerTurn("thread", turn)
	turn.setTurnID("current")
	request := protocol.ThreadTokenUsage{
		Last:  protocol.TokenUsageBreakdown{InputTokens: 200, CachedInputTokens: 100, OutputTokens: 7, TotalTokens: 207},
		Total: protocol.TokenUsageBreakdown{TotalTokens: 1207},
	}
	estimate := protocol.ThreadTokenUsage{Last: protocol.TokenUsageBreakdown{TotalTokens: 900}, Total: request.Total}
	for _, usage := range []protocol.ThreadTokenUsage{previous, request, request, estimate, estimate, request} {
		turn.handleNotification(&protocol.ThreadTokenUsageUpdatedNotification{ThreadID: "thread", TurnID: "current", TokenUsage: usage})
	}
	turn.handleNotification(&protocol.TurnCompletedNotification{ThreadID: "thread", Turn: protocol.Turn{ID: "current", Status: protocol.TurnStatusCompleted}})
	result, err := turn.result()
	if err != nil {
		t.Fatal(err)
	}
	// The final request repeats the first one's numbers after an estimate in
	// between, so it is a new request, not a re-send.
	want := sdk.Usage{
		InputTokens: 400, OutputTokens: 14, TotalTokens: 414, CachedInputTokens: 200,
		InputTokenDetails: sdk.InputTokenDetail{NoCacheTokens: 200, CacheReadTokens: 200},
	}
	if result.Usage == nil || *result.Usage != want {
		t.Fatalf("usage = %+v, want %+v", result.Usage, want)
	}
}

// After a restart the app server has no cached usage, and resuming the thread
// replays the earlier turn's usage before turn/started. The replay is the
// baseline, not a request of this turn.
func TestTurnUsageTakesAResumeReplayAsTheBaseline(t *testing.T) {
	srv := &appServer{turns: map[string]*turnState{}}
	turn := newTurnState(t.Context(), external.PromptInput{Sink: &captureSink{}}, "thread", nil, nil, nil, nil, slog.Default())
	defer turn.close()
	srv.registerTurn("thread", turn)
	replay := protocol.ThreadTokenUsage{
		Last:  protocol.TokenUsageBreakdown{InputTokens: 100, CachedInputTokens: 80, OutputTokens: 5, TotalTokens: 105},
		Total: protocol.TokenUsageBreakdown{TotalTokens: 5000},
	}
	request := protocol.ThreadTokenUsage{
		Last:  protocol.TokenUsageBreakdown{InputTokens: 200, CachedInputTokens: 100, OutputTokens: 7, TotalTokens: 207},
		Total: protocol.TokenUsageBreakdown{TotalTokens: 5207},
	}
	notify := func(turnID string, usage protocol.ThreadTokenUsage) {
		turn.handleNotification(&protocol.ThreadTokenUsageUpdatedNotification{ThreadID: "thread", TurnID: turnID, TokenUsage: usage})
	}
	notify("earlier", replay)
	turn.setTurnID("current")
	notify("current", replay)
	notify("current", request)
	notify("earlier", replay)
	turn.handleNotification(&protocol.TurnCompletedNotification{ThreadID: "thread", Turn: protocol.Turn{ID: "current", Status: protocol.TurnStatusCompleted}})
	result, err := turn.result()
	if err != nil {
		t.Fatal(err)
	}
	want := sdk.Usage{
		InputTokens: 200, OutputTokens: 7, TotalTokens: 207, CachedInputTokens: 100,
		InputTokenDetails: sdk.InputTokenDetail{NoCacheTokens: 100, CacheReadTokens: 100},
	}
	if result.Usage == nil || *result.Usage != want {
		t.Fatalf("usage = %+v, want %+v", result.Usage, want)
	}
}
