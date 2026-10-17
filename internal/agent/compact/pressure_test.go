package compact

import (
	"context"
	"testing"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

// lifecycleWithPlan is a run's lifecycle after its context plan priced the
// system prompt and tools at fixed tokens.
func lifecycleWithPlan(fixed int) *contextfrag.LifecycleHolder {
	holder := contextfrag.NewLifecycleHolder()
	holder.SetManifest(contextfrag.Manifest{BudgetPlan: &contextfrag.ContextBudgetPlan{Window: 8000, ActualSystemCost: fixed}})
	return holder
}

// A long run ends with far more context than it started with. The background
// trigger judges what the run grew to, not the pressure measured before it.
func TestRunEndCompactionJudgesWhatTheRunGrewTo(t *testing.T) {
	t.Parallel()

	service, runner := newControllerPolicyService(t, nil)
	req := runreq.ChatRequest{
		BotID:    "00000000-0000-0000-0000-000000000453",
		ThreadID: "00000000-0000-0000-0000-000000000454",
	}
	rc := runreq.ResolvedContext{
		ContextTokenBudget: 8000,
		Pressure:           runreq.NewRunPressure(lifecycleWithPlan(1000), conversationOf(1000)),
	}
	if _, ok := service.planAsync(context.Background(), req, rc, 0); ok {
		t.Fatal("planAsync() = true before the run grew, want the soft share not reached")
	}

	rc.Pressure.Observe(conversationOf(2000))
	rc.Pressure.Add(conversationOf(1200))
	runAutomaticCompaction(t, service, req, rc, 0)
	if len(runner.configs) == 0 {
		t.Fatal("no compaction ran for a run that ended past the soft share")
	}
	if got := runner.configs[0].TotalInputTokens; got < 4000 {
		t.Fatalf("compaction priced the run at %d tokens, want what it grew to (at least the 4000 soft share)", got)
	}
}

// The loop's own estimate replaces the run's at every call it offers, so a
// run's end starts from the conversation it last sent.
func TestStepCompactionReportsEachCallsConversation(t *testing.T) {
	t.Parallel()

	run, _, _ := newStepCompactionFixture(t, compaction.StatusOK)
	run.pressure = runreq.NewRunPressure(nil, nil)
	below := native.StepContextInput{FixedTokens: 100, Messages: conversationOf(300)}
	if got := run.compact(context.Background(), below); got != nil {
		t.Fatalf("messages = %q, want the conversation kept", sdkTexts(got))
	}
	if got, want := run.pressure.Tokens(), runreq.RequestInputTokens(0, below.Messages); got != want {
		t.Fatalf("run pressure = %d, want the call's conversation %d", got, want)
	}
}
