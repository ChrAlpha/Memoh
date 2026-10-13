package application

import (
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/turn"
)

const (
	syncCompactBotID    = "00000000-0000-0000-0000-000000000453"
	syncCompactThreadID = "00000000-0000-0000-0000-000000000454"
)

// syncCompactPressureMessages composes ~1000 estimator tokens of raw history,
// which sits above the 75% hard threshold of a 1000-token budget yet fits it.
func syncCompactPressureMessages() []turn.DiscussMessage {
	return []turn.DiscussMessage{
		{Role: "user", Content: strings.Repeat("a", 2000)},
		{Role: "assistant", Content: strings.Repeat("b", 2000)},
	}
}

func TestDiscussCompactionSeesSummaryAndDroppedHistoryPressure(t *testing.T) {
	for _, pressure := range []int{0, 3000} {
		service, _ := newControllerPolicyService(t, nil)
		runner := newGatedCompactionRunner()
		service.compactionService = runner
		ctx, endRun := WithRunEnd(t.Context())
		service.scheduleDiscussCompaction(ctx, turn.StartTurnCommand{
			BotID: syncCompactBotID, ThreadID: syncCompactThreadID, DiscussContextTokens: pressure,
			DiscussMessages: []turn.DiscussMessage{
				{Role: "user", Content: strings.Repeat("s", 2400), CompactionArtifactID: "summary"},
				{Role: "user", Content: strings.Repeat("r", 1200)},
			},
		}, ResolveRunConfigResult{ContextBudgetMaxTokens: 1000})
		runner.assertNoPass(t)
		endRun()
		cfg := runner.awaitPass(t)
		close(runner.release)
		if cfg.TotalInputTokens != max(900, pressure) || !cfg.HardPressure || cfg.ContextWindowTokens != 1000 {
			t.Fatalf("lost pre-selection pressure: %+v", cfg)
		}
	}
}

// A runtime without a model window measures against the absolute cap rather
// than leaving compaction off.
func TestDiscussCompactionMissingWindowUsesAbsoluteCap(t *testing.T) {
	t.Parallel()

	service, _ := newControllerPolicyService(t, nil)
	runner := newGatedCompactionRunner()
	t.Cleanup(func() { close(runner.release) })
	service.compactionService = runner
	service.SetContextAbsoluteMaxTokens(1000)
	ctx, endRun := WithRunEnd(t.Context())
	service.scheduleDiscussCompaction(ctx, turn.StartTurnCommand{
		BotID:           syncCompactBotID,
		ThreadID:        syncCompactThreadID,
		DiscussMessages: syncCompactPressureMessages(),
	}, ResolveRunConfigResult{})
	runner.assertNoPass(t)
	endRun()
	if cfg := runner.awaitPass(t); cfg.ContextWindowTokens != 1000 || !cfg.HardPressure {
		t.Fatalf("background pass = %+v, want hard pressure against the 1000-token cap", cfg)
	}
}
