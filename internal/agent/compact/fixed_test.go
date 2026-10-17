package compact

import (
	"context"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

// The sync compaction hands the fixed part of the judged request to the
// engine, so the rows are aimed at the target less it.
func TestRunCompactionSyncFixedCarriesTheFixedRequestIntoTheConfig(t *testing.T) {
	t.Parallel()

	service, runner := newControllerPolicyService(t, nil)
	req := runreq.ChatRequest{
		BotID:    "00000000-0000-0000-0000-000000000453",
		ThreadID: "00000000-0000-0000-0000-000000000454",
	}
	service.runSync(context.Background(), req, 9000, 3000, 8000, "")
	if len(runner.configs) == 0 {
		t.Fatal("no compaction reached the engine")
	}
	cfg := runner.configs[0]
	if cfg.FixedTokens != 3000 {
		t.Fatalf("FixedTokens = %d, want the fixed request 3000", cfg.FixedTokens)
	}
	if want := syncBackstopTargetTokens(nil, 8000); cfg.TargetTokens != want {
		t.Fatalf("TargetTokens = %d, want the backstop target %d", cfg.TargetTokens, want)
	}
}

// At run end the fixed part is the judged request less the rows it holds,
// so a run whose system prompt and tools are large gets a large fixed part.
func TestRunEndCompactionCarriesTheRunsFixedPart(t *testing.T) {
	t.Parallel()

	service, runner := newControllerPolicyService(t, nil)
	req := runreq.ChatRequest{
		BotID:    "00000000-0000-0000-0000-000000000453",
		ThreadID: "00000000-0000-0000-0000-000000000454",
	}
	rc := runreq.ResolvedContext{
		ContextTokenBudget: 8000,
		Pressure:           runreq.NewRunPressure(lifecycleWithPlan(3000), conversationOf(3000)),
	}
	rc.Pressure.Observe(conversationOf(2500))
	runAutomaticCompaction(t, service, req, rc, 0)
	if len(runner.configs) == 0 {
		t.Fatal("no compaction ran for a run past the soft share")
	}
	if got := runner.configs[0].FixedTokens; got < 3000 {
		t.Fatalf("run-end FixedTokens = %d, want at least the run's 3000-token system and tools", got)
	}
}

// Inside a run the fixed part is the loop's own fixed cost plus the messages
// no stored row holds, here the run's extras and its unstored user message.
func TestStepCompactionFixedPartCountsTheRunsOwnMessages(t *testing.T) {
	t.Parallel()

	extra := sdk.UserMessage("workspace notice: " + string(make([]byte, 400)))
	request := sdk.UserMessage("the user's message, not yet a row")
	run := &stepRun{
		requestMessageID: func() string { return "" },
		extras:           []sdk.Message{extra},
		request:          []sdk.Message{request},
	}
	input := native.StepContextInput{FixedTokens: 1000}
	want := 1000 + runreq.RequestInputTokens(0, []sdk.Message{extra, request})
	if got := run.fixedTokens(input); got != want {
		t.Fatalf("fixedTokens = %d, want %d (fixed 1000 plus the extras and request)", got, want)
	}
}

// A replacement names the message it replaces; the sync compaction it runs
// carries that cutoff into the engine, so no summary of the replaced turn is
// read as prior context.
func TestRunCompactionSyncFixedCarriesTheReplacementCutoff(t *testing.T) {
	t.Parallel()

	service, runner := newControllerPolicyService(t, nil)
	req := runreq.ChatRequest{
		BotID:                        "00000000-0000-0000-0000-000000000453",
		ThreadID:                     "00000000-0000-0000-0000-000000000454",
		HistoryCutoffBeforeMessageID: "00000000-0000-0000-0000-000000000455",
	}
	service.runSync(context.Background(), req, 9000, 0, 8000, "")
	if len(runner.configs) == 0 {
		t.Fatal("no compaction reached the engine")
	}
	if runner.configs[0].HistoryCutoff == nil {
		t.Fatal("a replacement's compaction ran without its history cutoff")
	}
}
