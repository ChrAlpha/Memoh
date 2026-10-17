package compact

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/context/compaction"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	agenttools "github.com/felinics/memoh/internal/agent/tool"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// The rows of a session can sit under the hard share while the whole first
// request does not: the system prompt, tools and the user's message are only
// priced once the loop holds them. The first call is judged on what it sends.
func TestFirstAttemptOverHardShareCompactsBeforeTheProviderCall(t *testing.T) {
	t.Parallel()

	run, runner, history := newStepCompactionFixture(t, compaction.StatusOK)
	runner.onRun = history.markCompacted
	run.budget = 1000
	run.requestMessageID = func() string { return "" }
	run.request = []sdk.Message{sdk.UserMessage("the request")}
	history.big = []messagepkg.Message{agenttest.AssembleText(t, "old-step", "assistant", "old step", agenttest.AssembleBase)}
	history.small = []messagepkg.Message{agenttest.AssembleText(t, "summary", "assistant", "<summary>short</summary>", agenttest.AssembleBase)}

	request := sdk.UserMessage(strings.Repeat("the request ", 600))
	model := &scriptedModel{}
	agent := native.New(native.Deps{})
	agent.SetToolProviders([]agenttools.ToolProvider{bulkyToolProvider{}})
	cfg := native.RunConfig{
		Model:                &sdk.Model{ID: "scripted-model", Provider: model},
		Messages:             []sdk.Message{request},
		SupportsToolCall:     true,
		Identity:             native.SessionContext{BotID: run.req.BotID},
		StepContextCompactor: run.compact,
	}
	for range agent.Stream(context.Background(), cfg) { //nolint:revive // the events are not under test
	}

	if len(model.requests) == 0 {
		t.Fatal("the run never reached the provider")
	}
	first := sdkTexts(model.requests[0].Messages)
	if len(first) != 3 || first[0] != "<summary>short</summary>" || first[1] != "recalled memory" {
		t.Fatalf("first request = %q, want the summary, the run's extras and the request, the raw rows compacted away", first)
	}
	if runner.callCount() == 0 {
		t.Fatal("no compaction ran before the first call, want one")
	}
}

// After a pre-turn compaction the first call's conversation is the summary
// and the request, under the hard share: the first attempt does not compact
// again.
func TestFirstAttemptAfterACompactedTurnIsNotCompactedAgain(t *testing.T) {
	t.Parallel()

	run, runner, _ := newStepCompactionFixture(t, compaction.StatusOK)
	run.budget = 1000
	small := []sdk.Message{sdk.UserMessage("<summary>short</summary>"), sdk.UserMessage("the request")}
	if got := run.compact(context.Background(), native.StepContextInput{FixedTokens: 100, Messages: small}); got != nil {
		t.Fatalf("messages = %q, want the conversation kept", sdkTexts(got))
	}
	if runner.callCount() != 0 {
		t.Fatalf("compaction ran %d times, want none below the hard share", runner.callCount())
	}
}
