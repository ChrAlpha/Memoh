package application

import (
	"context"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/turn"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
)

const (
	syncCompactBotID    = "00000000-0000-0000-0000-000000000453"
	syncCompactThreadID = "00000000-0000-0000-0000-000000000454"
)

func TestDiscussRecoveryTargetsSpaceRemainingAfterCurrentInput(t *testing.T) {
	for _, runtimeType := range []string{sessionpkg.RuntimeModel, sessionpkg.RuntimeACPAgent} {
		service, runner := newControllerPolicyService(t, nil)
		fired := service.maybeSyncCompactDiscuss(t.Context(), turn.StartTurnCommand{
			BotID: syncCompactBotID, ThreadID: syncCompactThreadID,
			DiscussMessages: []turn.DiscussMessage{
				{Role: "user", Content: strings.Repeat("s", 1400), CompactionArtifactID: "summary"},
				{Role: "user", Content: strings.Repeat("c", 3000)},
			},
		}, ResolveRunConfigResult{ContextBudgetMaxTokens: 1000, RuntimeType: runtimeType}, "recovery")
		if !fired || len(runner.configs) != 1 || runner.configs[0].HistoryBudgetTokens >= 625 {
			t.Fatalf("recovery ignored current input: runtime=%s fired=%v configs=%+v", runtimeType, fired, runner.configs)
		}
	}
}

func TestDiscussCurrentInputAloneTooLargeDoesNotRunCompaction(t *testing.T) {
	service, runner := newControllerPolicyService(t, nil)
	fired := service.maybeSyncCompactDiscuss(t.Context(), turn.StartTurnCommand{
		BotID: syncCompactBotID, ThreadID: syncCompactThreadID,
		DiscussMessages: []turn.DiscussMessage{{Role: "user", Content: strings.Repeat("c", 4004)}},
	}, ResolveRunConfigResult{ContextBudgetMaxTokens: 1000}, "recovery")
	if fired || len(runner.configs) != 0 {
		t.Fatalf("irreducible current input ran compaction: fired=%v calls=%d", fired, len(runner.configs))
	}
	fired = service.maybeSyncCompactDiscuss(t.Context(), turn.StartTurnCommand{
		BotID: syncCompactBotID, ThreadID: syncCompactThreadID,
		DiscussContextOverflow: true, DiscussContextTokens: 3000, DiscussCurrentTokens: 1200,
	}, ResolveRunConfigResult{ContextBudgetMaxTokens: 1000}, "smaller-model-window")
	if fired || len(runner.configs) != 0 {
		t.Fatal("Channel metadata did not respect the smaller resolved model budget")
	}
}

func TestDiscussOversizedBatchCompactsOlderInputBehindProtectedSuffix(t *testing.T) {
	service, runner := newControllerPolicyService(t, nil)
	var messages []turn.DiscussMessage
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		messages = append(messages, turn.DiscussMessage{Role: "user", Content: strings.Repeat(id, 1000), Source: &turn.ContextMessageSource{Kind: "external", ID: id, Current: true}})
	}
	fired := service.maybeSyncCompactDiscuss(t.Context(), turn.StartTurnCommand{BotID: syncCompactBotID, ThreadID: syncCompactThreadID, DiscussMessages: messages}, ResolveRunConfigResult{ContextBudgetMaxTokens: 1000}, "recovery")
	if !fired || len(runner.configs) != 1 {
		t.Fatalf("an omitted older input must be compacted: fired=%v calls=%d", fired, len(runner.configs))
	}
	cfg := runner.configs[0]
	var protected []string
	for _, source := range cfg.ProtectedSources {
		protected = append(protected, source.ID)
	}
	if strings.Join(protected, ",") != "c,d,e" || cfg.HistoryBudgetTokens != 250 {
		t.Fatalf("protected=%v history_budget=%d, want the newest suffix within 900 and its cost counted once", protected, cfg.HistoryBudgetTokens)
	}
	fired = service.maybeSyncCompactDiscuss(t.Context(), turn.StartTurnCommand{BotID: syncCompactBotID, ThreadID: syncCompactThreadID, DiscussMessages: messages[2:], DiscussOmittedSources: []turn.ContextMessageSource{*messages[0].Source, *messages[1].Source}}, ResolveRunConfigResult{ContextBudgetMaxTokens: 1000}, "channel-omission")
	if !fired || len(runner.configs) != 2 || len(runner.configs[1].ProtectedSources) != 3 {
		t.Fatalf("input Channel omitted must be compacted too: fired=%v configs=%+v", fired, runner.configs)
	}
}

func TestDiscussAdmissionOverflowRunsRecovery(t *testing.T) {
	service, runner := newControllerPolicyService(t, nil)
	fired := service.maybeSyncCompactDiscuss(t.Context(), turn.StartTurnCommand{
		BotID: syncCompactBotID, ThreadID: syncCompactThreadID,
		DiscussContextOverflow: true, DiscussContextTokens: 2000, DiscussCurrentTokens: 500,
	}, ResolveRunConfigResult{ContextBudgetMaxTokens: 1000}, "recovery")
	if !fired || len(runner.configs) != 1 || runner.configs[0].HistoryBudgetTokens != 500 {
		t.Fatalf("rejected context did not enter recovery: fired=%v configs=%+v", fired, runner.configs)
	}
}

func TestDiscussRecoveryHonorsCancellation(t *testing.T) {
	service, runner := newControllerPolicyService(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	fired := service.maybeSyncCompactDiscuss(ctx, turn.StartTurnCommand{
		BotID: syncCompactBotID, ThreadID: syncCompactThreadID,
		DiscussContextOverflow: true, DiscussContextTokens: 2000, DiscussCurrentTokens: 500,
	}, ResolveRunConfigResult{ContextBudgetMaxTokens: 1000}, "recovery")
	if fired || len(runner.configs) != 0 {
		t.Fatalf("cancelled recovery ran: configs=%+v", runner.configs)
	}
}

func TestMaybeSyncCompactDiscussFittingInputNoop(t *testing.T) {
	t.Parallel()

	service, runner := newControllerPolicyService(t, nil)
	fired := service.maybeSyncCompactDiscuss(context.Background(), turn.StartTurnCommand{
		BotID:           syncCompactBotID,
		ThreadID:        syncCompactThreadID,
		DiscussMessages: []turn.DiscussMessage{{Role: "user", Content: "small"}},
	}, ResolveRunConfigResult{ContextBudgetMaxTokens: 1000}, "run-1")

	if fired || len(runner.configs) != 0 {
		t.Fatalf("fitting input must not compact: fired=%v runs=%d", fired, len(runner.configs))
	}
}

func TestMaybeSyncCompactDiscussMissingWindowUsesAbsoluteCap(t *testing.T) {
	t.Parallel()

	service, runner := newControllerPolicyService(t, nil)
	service.SetContextAbsoluteMaxTokens(1000)
	var messages []turn.DiscussMessage
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		messages = append(messages, turn.DiscussMessage{Role: "user", Content: strings.Repeat(id, 1000), Source: &turn.ContextMessageSource{Kind: "external", ID: id, Current: true}})
	}
	fired := service.maybeSyncCompactDiscuss(context.Background(), turn.StartTurnCommand{
		BotID:           syncCompactBotID,
		ThreadID:        syncCompactThreadID,
		DiscussMessages: messages,
	}, ResolveRunConfigResult{}, "run-1")

	if !fired || len(runner.configs) != 1 || runner.configs[0].HistoryBudgetTokens != 250 {
		t.Fatalf("a missing model window must fall back to the absolute cap: fired=%v configs=%+v", fired, runner.configs)
	}
}
