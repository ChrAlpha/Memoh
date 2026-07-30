package native

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/agent/sessionmode"
	tools "github.com/memohai/memoh/internal/agent/tool"
)

// currentTimeLine extracts the "Current time: ..." line a materialized spawn
// query must be prefixed with, failing the test if the prefix is missing.
func currentTimeLine(t *testing.T, text string) string {
	t.Helper()
	const prefix = "Current time: "
	if !strings.HasPrefix(text, prefix) {
		t.Fatalf("expected message to start with %q, got %q", prefix, text)
	}
	line, _, _ := strings.Cut(text, "\n")
	return strings.TrimPrefix(line, prefix)
}

func TestSpawnAdapterPrefixesQueryWithCurrentTime(t *testing.T) {
	t.Parallel()
	modelProvider := &usageRecordingProvider{}
	recorder := &applierRecorder{}
	a := newApplierTestAgent(recorder)
	adapter := NewSpawnAdapter(a)

	before := time.Now()
	if _, err := adapter.Generate(context.Background(), tools.SpawnRunConfig{
		Model: &sdk.Model{
			ID:       "spawn-time-model",
			Provider: modelProvider,
			Type:     sdk.ModelTypeChat,
		},
		System: "subagent system",
		Query:  "do the task",
		Identity: tools.SpawnIdentity{
			BotID:      "bot-1",
			SessionID:  "session-1",
			IsSubagent: true,
		},
	}); err != nil {
		t.Fatalf("spawn Generate error: %v", err)
	}
	after := time.Now()

	_, seen := recorder.snapshot()
	if len(seen.Messages) == 0 {
		t.Fatal("expected the spawn query to be materialized into RunConfig.Messages")
	}
	text := textOfMessage(seen.Messages[len(seen.Messages)-1])

	if !strings.Contains(text, "do the task") {
		t.Fatalf("expected message to retain the original query text, got %q", text)
	}

	timeLine := currentTimeLine(t, text)
	parsed, err := time.Parse(time.RFC3339, timeLine)
	if err != nil {
		t.Fatalf("expected current time line to parse as RFC3339, got %q: %v", timeLine, err)
	}
	if parsed.Before(before.Add(-time.Second)) || parsed.After(after.Add(time.Second)) {
		t.Fatalf("parsed time %v not within window [%v, %v]", parsed, before, after)
	}
}

func TestSpawnAdapterUsesIdentityTimezoneForCurrentTime(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	modelProvider := &usageRecordingProvider{}
	recorder := &applierRecorder{}
	a := newApplierTestAgent(recorder)
	adapter := NewSpawnAdapter(a)

	if _, err := adapter.Generate(context.Background(), tools.SpawnRunConfig{
		Model: &sdk.Model{
			ID:       "spawn-tz-model",
			Provider: modelProvider,
			Type:     sdk.ModelTypeChat,
		},
		System: "subagent system",
		Query:  "do the task",
		Identity: tools.SpawnIdentity{
			BotID:            "bot-1",
			SessionID:        "session-1",
			IsSubagent:       true,
			TimezoneLocation: loc,
		},
	}); err != nil {
		t.Fatalf("spawn Generate error: %v", err)
	}

	_, seen := recorder.snapshot()
	text := textOfMessage(seen.Messages[len(seen.Messages)-1])
	timeLine := currentTimeLine(t, text)
	if !strings.HasSuffix(timeLine, "+08:00") {
		t.Fatalf("expected current time line to carry the Asia/Shanghai offset, got %q", timeLine)
	}
}

func TestSpawnSystemPromptOmitsCurrentTime(t *testing.T) {
	t.Parallel()
	prompt := SpawnSystemPrompt(sessionmode.Subagent)
	if strings.Contains(prompt, "Current time") {
		t.Fatalf("expected subagent system prompt to stay free of current time, got:\n%s", prompt)
	}
}

// TestSpawnAdapterBuildsFragmentsFirstContextSourceFrags proves a subagent
// run populates RunConfig.ContextSourceFrags directly (typed system sections
// plus history), instead of leaving it empty and falling back to the legacy
// reverse-parse of cfg.System inside contextview.ApplyProviderRunConfig. The
// query is pre-materialized into Messages by runConfigFromSpawnRunConfig
// before frags are built, so it must ride as the one trailing current-user
// fragment, not as a second, separate query fragment.
func TestSpawnAdapterBuildsFragmentsFirstContextSourceFrags(t *testing.T) {
	t.Parallel()
	modelProvider := &usageRecordingProvider{}
	recorder := &applierRecorder{}
	adapter := NewSpawnAdapter(newApplierTestAgent(recorder))

	history := []sdk.Message{
		sdk.UserMessage("earlier question"),
		sdk.AssistantMessage("earlier answer"),
	}

	if _, err := adapter.Generate(context.Background(), tools.SpawnRunConfig{
		Model:       &sdk.Model{ID: "spawn-frags-model", Provider: modelProvider, Type: sdk.ModelTypeChat},
		System:      SpawnSystemPrompt(sessionmode.Subagent),
		Query:       "do the task",
		Messages:    history,
		SessionType: sessionmode.Subagent,
		Identity:    tools.SpawnIdentity{BotID: "bot-1", SessionID: "session-1", IsSubagent: true},
	}); err != nil {
		t.Fatalf("spawn Generate error: %v", err)
	}

	_, seen := recorder.snapshot()
	frags := seen.ContextSourceFrags
	if len(frags) == 0 {
		t.Fatal("expected a spawn run to populate ContextSourceFrags")
	}

	wantSections := map[string]contextfrag.Kind{
		"system.prompt.intro": contextfrag.KindSystemPrompt,
		"system.bot_identity": contextfrag.KindBotIdentity,
		"system.prompt.body":  contextfrag.KindSystemPrompt,
		"system.prompt.tail":  contextfrag.KindSystemPrompt,
	}
	seenSections := make(map[string]bool, len(wantSections))
	var historyMessages []sdk.Message
	var currentMessages []sdk.Message
	for _, frag := range frags {
		if kind, ok := wantSections[frag.ID]; ok {
			seenSections[frag.ID] = true
			if frag.Kind != kind || frag.Slot != contextfrag.SlotSystem {
				t.Fatalf("section %s = {Kind:%s Slot:%s}, want {Kind:%s Slot:system}", frag.ID, frag.Kind, frag.Slot, kind)
			}
		}
		if frag.Slot == contextfrag.SlotHistory {
			if msg := frag.Parts[0].Message; msg != nil {
				historyMessages = append(historyMessages, *msg)
			}
		}
		if frag.Slot == contextfrag.SlotCurrentUser {
			if frag.Kind != contextfrag.KindCurrentUserMessage {
				t.Fatalf("current-user frag kind = %q, want %q: %#v", frag.Kind, contextfrag.KindCurrentUserMessage, frag)
			}
			if msg := frag.Parts[0].Message; msg != nil {
				currentMessages = append(currentMessages, *msg)
			}
		}
	}
	for id := range wantSections {
		if !seenSections[id] {
			t.Fatalf("missing expected system section %q in %#v", id, frags)
		}
	}
	if len(historyMessages) != 2 {
		t.Fatalf("history messages = %d, want the 2 given messages, got %#v", len(historyMessages), historyMessages)
	}
	if text := textOfMessage(historyMessages[0]); text != "earlier question" {
		t.Fatalf("history[0] = %q, want %q", text, "earlier question")
	}
	if text := textOfMessage(historyMessages[1]); text != "earlier answer" {
		t.Fatalf("history[1] = %q, want %q", text, "earlier answer")
	}
	if len(currentMessages) != 1 {
		t.Fatalf("current-user messages = %d, want the one materialized query, got %#v", len(currentMessages), currentMessages)
	}
	current := textOfMessage(currentMessages[0])
	if !strings.HasPrefix(current, "Current time: ") || !strings.HasSuffix(current, "do the task") {
		t.Fatalf("current-user materialized query = %q, want a %q-prefixed message ending in the query", current, "Current time: ")
	}
}

// TestSpawnAdapterHistoryFragsPinnedAgainstOverflow verifies that subagent
// history fragments are pinned against budget-driven overflow trimming:
// runConfigFromSpawnRunConfig never sets ContextTrimmableMessages, so under
// the legacy path every subagent history message is implicitly "must keep"
// (HistoryMessagesCollector marks index >= TrimmablePrefix(=0), i.e. every
// index). contextfrag.CompileFrags does not set Budget at all, so the
// fragments-first path must mark it explicitly or a budget-constrained
// subagent run could silently trim history that never gets trimmed today.
// The appended query is a SlotCurrentUser fragment and must not be counted as
// or pinned through the history path.
func TestSpawnAdapterHistoryFragsPinnedAgainstOverflow(t *testing.T) {
	t.Parallel()
	modelProvider := &usageRecordingProvider{}
	recorder := &applierRecorder{}
	adapter := NewSpawnAdapter(newApplierTestAgent(recorder))

	if _, err := adapter.Generate(context.Background(), tools.SpawnRunConfig{
		Model: &sdk.Model{ID: "spawn-budget-model", Provider: modelProvider, Type: sdk.ModelTypeChat},
		Query: "keep everything",
		Messages: []sdk.Message{
			sdk.UserMessage("m1"),
			sdk.AssistantMessage("m2"),
			sdk.UserMessage("m3"),
		},
		SessionType: sessionmode.Subagent,
		Identity:    tools.SpawnIdentity{BotID: "bot-1", SessionID: "session-1", IsSubagent: true},
	}); err != nil {
		t.Fatalf("spawn Generate error: %v", err)
	}

	_, seen := recorder.snapshot()
	historyCount := 0
	currentCount := 0
	for _, frag := range seen.ContextSourceFrags {
		if frag.Slot == contextfrag.SlotCurrentUser {
			currentCount++
			continue
		}
		if frag.Slot != contextfrag.SlotHistory {
			continue
		}
		historyCount++
		if frag.Budget.Overflow != contextfrag.OverflowKeep {
			t.Fatalf("history frag %s Budget.Overflow = %q, want %q (subagents never set ContextTrimmableMessages, so every history message must be pinned)",
				frag.ID, frag.Budget.Overflow, contextfrag.OverflowKeep)
		}
	}
	if historyCount != 3 {
		t.Fatalf("history frag count = %d, want 3", historyCount)
	}
	if currentCount != 1 {
		t.Fatalf("current-user frag count = %d, want 1 materialized query", currentCount)
	}
}

// TestSpawnAdapterRepairsDanglingToolCallInHistory verifies that the
// fragments-first path repairs dangling tool calls in subagent history:
// subagent history comes from a raw, unsanitized DB load
// (tools.SpawnProvider.loadAgentMessages), which can legitimately contain a
// dangling assistant tool-call with no matching tool-result (e.g. a prior
// subagent turn was interrupted mid-call). Sending that dangling call to most
// providers is a hard API error, so the fragments-first path must repair it
// the same way contextview.HistoryMessagesCollector already does today.
func TestSpawnAdapterRepairsDanglingToolCallInHistory(t *testing.T) {
	t.Parallel()
	modelProvider := &usageRecordingProvider{}
	recorder := &applierRecorder{}
	adapter := NewSpawnAdapter(newApplierTestAgent(recorder))

	dangling := sdk.Message{
		Role: sdk.MessageRoleAssistant,
		Content: []sdk.MessagePart{
			sdk.ToolCallPart{ToolCallID: "call-lost", ToolName: "web_search", Input: map[string]any{}},
		},
	}
	history := []sdk.Message{
		sdk.UserMessage("question"),
		dangling,
		sdk.UserMessage("next question"),
	}

	if _, err := adapter.Generate(context.Background(), tools.SpawnRunConfig{
		Model:       &sdk.Model{ID: "spawn-repair-model", Provider: modelProvider, Type: sdk.ModelTypeChat},
		Messages:    history,
		SessionType: sessionmode.Subagent,
		Identity:    tools.SpawnIdentity{BotID: "bot-1", SessionID: "session-1", IsSubagent: true},
	}); err != nil {
		t.Fatalf("spawn Generate error: %v", err)
	}

	_, seen := recorder.snapshot()
	var historyFrags []contextfrag.ContextFrag
	for _, frag := range seen.ContextSourceFrags {
		if frag.Slot == contextfrag.SlotHistory {
			historyFrags = append(historyFrags, frag)
		}
	}
	if len(historyFrags) != 4 {
		t.Fatalf("history frags = %d, want 4 (question, dangling call, synthetic repair, next question), got %#v", len(historyFrags), historyFrags)
	}
	synthetic := historyFrags[2]
	msg := synthetic.Parts[0].Message
	if msg == nil || msg.Role != sdk.MessageRoleTool {
		t.Fatalf("frag after the dangling call must be a synthetic tool result: %#v", synthetic)
	}
	result, ok := msg.Content[0].(sdk.ToolResultPart)
	if !ok || result.ToolCallID != "call-lost" || !result.IsError {
		t.Fatalf("synthetic result must close the dangling call as an error: %#v", msg.Content[0])
	}
	if !strings.Contains(synthetic.Provenance.Source, "repair") {
		t.Fatalf("synthetic closure must be attributed to the repair policy: %+v", synthetic.Provenance)
	}
}

// TestSpawnAdapterSkipsToolUsageSystemSpliceOnceFragsFirst confirms the
// interaction with agent.go's legacy tool-usage guard (runStream/runGenerate,
// "if len(cfg.ContextSourceFrags) == 0 { cfg.System = appendToolUsageToSystem(...) }"):
// now that spawn runs always populate ContextSourceFrags, that guard's
// condition is always false for them, so tool usage must flow only through
// ContextToolUsage (consumed by the view as its own fragment) and never get
// spliced into System — mirroring TestGenerateFragsFirstHandsToolUsageToView's
// property for non-spawn runs.
func TestSpawnAdapterSkipsToolUsageSystemSpliceOnceFragsFirst(t *testing.T) {
	t.Parallel()
	modelProvider := &usageRecordingProvider{}
	recorder := &applierRecorder{}
	adapter := NewSpawnAdapter(newApplierTestAgent(recorder, &usageTestProvider{emitTool: true, usage: usageMarker}))

	if _, err := adapter.Generate(context.Background(), tools.SpawnRunConfig{
		Model:            &sdk.Model{ID: "spawn-toolusage-model", Provider: modelProvider, Type: sdk.ModelTypeChat},
		Query:            "do the task",
		SessionType:      sessionmode.Subagent,
		SupportsToolCall: true,
		Identity:         tools.SpawnIdentity{BotID: "bot-1", SessionID: "session-1", IsSubagent: true},
	}); err != nil {
		t.Fatalf("spawn Generate error: %v", err)
	}

	_, seen := recorder.snapshot()
	if len(seen.ContextSourceFrags) == 0 {
		t.Fatal("expected ContextSourceFrags to be populated")
	}
	if !strings.Contains(seen.ContextToolUsage, usageMarker) {
		t.Fatalf("expected ContextToolUsage to carry the usage marker, got %q", seen.ContextToolUsage)
	}
	if strings.Contains(seen.System, usageMarker) {
		t.Fatalf("expected System to stay untouched by the legacy splice guard since ContextSourceFrags is non-empty, got %q", seen.System)
	}
}

// TestSpawnAdapterGeneratePopulatesContextLifecycle verifies that a spawn's
// RunConfig gets its own contextfrag.LifecycleHolder (runConfigFromSpawnRunConfig
// used to leave rc.ContextLifecycle nil, so SetManifest was a no-op and the
// snapshot was silently lost) and that Generate surfaces the resulting
// snapshot on tools.SpawnResult with sane counts.
func TestSpawnAdapterGeneratePopulatesContextLifecycle(t *testing.T) {
	t.Parallel()
	modelProvider := &usageRecordingProvider{}
	adapter := NewSpawnAdapter(newTestAgent())

	result, err := adapter.Generate(context.Background(), tools.SpawnRunConfig{
		Model:       &sdk.Model{ID: "spawn-lifecycle-model", Provider: modelProvider, Type: sdk.ModelTypeChat},
		Query:       "do the task",
		SessionType: sessionmode.Subagent,
		Identity:    tools.SpawnIdentity{BotID: "bot-1", SessionID: "session-1", IsSubagent: true},
	})
	if err != nil {
		t.Fatalf("spawn Generate error: %v", err)
	}
	if result.ContextLifecycle == nil {
		t.Fatal("expected SpawnResult.ContextLifecycle to be populated")
	}
	if result.ContextLifecycle.Counts.Fragments == 0 || result.ContextLifecycle.Counts.Messages == 0 {
		t.Fatalf("expected non-zero manifest counts, got %+v", result.ContextLifecycle.Counts)
	}
}

// TestSpawnAdapterGenerateWithWatchdogPopulatesContextLifecycle mirrors
// TestSpawnAdapterGeneratePopulatesContextLifecycle for the streaming
// (GenerateWithWatchdog) spawn adapter path used by the subagent tool.
func TestSpawnAdapterGenerateWithWatchdogPopulatesContextLifecycle(t *testing.T) {
	t.Parallel()
	modelProvider := &atomicMockProvider{
		handler: func(_ int, _ sdk.GenerateParams) (*sdk.GenerateResult, error) {
			return &sdk.GenerateResult{Text: "ok", FinishReason: sdk.FinishReasonStop}, nil
		},
	}
	adapter := NewSpawnAdapter(newTestAgent())

	result, err := adapter.GenerateWithWatchdog(context.Background(), tools.SpawnRunConfig{
		Model:       &sdk.Model{ID: "spawn-lifecycle-watchdog-model", Provider: modelProvider, Type: sdk.ModelTypeChat},
		Query:       "do the task",
		SessionType: sessionmode.Subagent,
		Identity:    tools.SpawnIdentity{BotID: "bot-1", SessionID: "session-1", IsSubagent: true},
	}, func() {})
	if err != nil {
		t.Fatalf("spawn GenerateWithWatchdog error: %v", err)
	}
	if result.ContextLifecycle == nil {
		t.Fatal("expected SpawnResult.ContextLifecycle to be populated")
	}
	if result.ContextLifecycle.Counts.Fragments == 0 || result.ContextLifecycle.Counts.Messages == 0 {
		t.Fatalf("expected non-zero manifest counts, got %+v", result.ContextLifecycle.Counts)
	}
}

func TestSpawnAdapterGenerateWithWatchdogFailsOnStreamError(t *testing.T) {
	t.Parallel()

	plan := contextfrag.ContextBudgetPlan{
		Window:           4096,
		OutputReserve:    8192,
		ActualSystemCost: 123,
	}
	a := New(Deps{
		ContextViewApplier: failingBudgetApplier(plan, contextfrag.ErrBudgetUnsatisfied),
	})
	adapter := NewSpawnAdapter(a)

	result, err := adapter.GenerateWithWatchdog(context.Background(), tools.SpawnRunConfig{
		Model: &sdk.Model{
			ID:       "spawn-preflight-error",
			Provider: &preflightCountingProvider{},
			Type:     sdk.ModelTypeChat,
		},
		Query: "do the task",
	}, func() {})

	if result == nil || result.ContextLifecycle == nil {
		t.Fatalf("GenerateWithWatchdog result = %#v, want failure lifecycle audit", result)
	}
	if got := result.ContextLifecycle.BudgetPlan; got == nil || got.ActualSystemCost != plan.ActualSystemCost {
		t.Fatalf("failure lifecycle budget plan = %#v, want ActualSystemCost %d", got, plan.ActualSystemCost)
	}
	if err == nil || err.Error() != publicBudgetUnsatisfiedError {
		t.Fatalf("GenerateWithWatchdog error = %v, want public context-budget failure", err)
	}
}

func TestSpawnAdapterGenerateFailureCarriesContextLifecycle(t *testing.T) {
	t.Parallel()

	plan := contextfrag.ContextBudgetPlan{
		Window:           4096,
		OutputReserve:    8192,
		ActualSystemCost: 321,
	}
	a := New(Deps{
		ContextViewApplier: failingBudgetApplier(plan, contextfrag.ErrBudgetUnsatisfied),
	})
	adapter := NewSpawnAdapter(a)

	result, err := adapter.Generate(context.Background(), tools.SpawnRunConfig{
		Model: &sdk.Model{
			ID:       "spawn-generate-preflight-error",
			Provider: &preflightCountingProvider{},
			Type:     sdk.ModelTypeChat,
		},
		Query: "do the task",
	})

	if !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) {
		t.Fatalf("Generate error = %v, want ErrBudgetUnsatisfied", err)
	}
	if result == nil || result.ContextLifecycle == nil {
		t.Fatalf("Generate result = %#v, want failure lifecycle audit", result)
	}
	if got := result.ContextLifecycle.BudgetPlan; got == nil || got.ActualSystemCost != plan.ActualSystemCost {
		t.Fatalf("failure lifecycle budget plan = %#v, want ActualSystemCost %d", got, plan.ActualSystemCost)
	}
}
