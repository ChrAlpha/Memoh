package contextview

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	native "github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/toolexec"
)

func recoveryBudgetFixture() native.RunConfig {
	history := stableHistoryMessageFrag("history", sdk.AssistantMessage(strings.Repeat("h", 8000)))
	return native.RunConfig{
		ContextBudgetMaxTokens:  16000,
		ContextToolDefsResolved: true,
		ContextToolDefs:         []contextfrag.ToolDefAccounting{{Name: "large_roster", TokenEstimate: 10000}},
		ContextSourceFrags: []contextfrag.ContextFrag{
			systemTextFrag("system", strings.Repeat("s", 3200), contextfrag.KindSystemPrompt, 20),
			history,
			currentMessageFrag("current", "continue"),
		},
		ContextLifecycle: contextfrag.NewLifecycleHolder(),
	}
}

func TestProviderBudgetRecoveryUsesFinalHistoryBudget(t *testing.T) {
	cfg := recoveryBudgetFixture()
	calls := 0
	cfg.RecoverContextBudget = func(_ context.Context, failed native.RunConfig) (native.RunConfig, bool, error) {
		calls++
		plan := failed.ContextManifest.BudgetPlan
		if plan == nil || plan.HistoryBudget != 998 || plan.ToolDefsCost != 10000 || plan.ActualSystemCost != 1000 {
			t.Fatalf("recovery budget = %+v", plan)
		}
		failed.ContextSourceFrags = append([]contextfrag.ContextFrag(nil), failed.ContextSourceFrags...)
		failed.ContextSourceFrags[1] = stableHistoryMessageFrag("summary", sdk.AssistantMessage("earlier conversation summary"))
		return failed, true, nil
	}
	got, err := ProviderRunConfigApplier(nil)(context.Background(), cfg)
	if err != nil {
		t.Fatalf("context must be admitted after compaction: %v", err)
	}
	if calls != 1 || !reflect.DeepEqual(got.Messages, []sdk.Message{sdk.AssistantMessage("earlier conversation summary"), sdk.UserMessage("continue")}) {
		t.Fatalf("recovered calls=%d messages=%+v", calls, got.Messages)
	}
	for _, mutation := range got.ContextMutations.Records() {
		if mutation.Kind == contextfrag.MutationContextBudgetFailure {
			t.Fatal("successful recovery must not publish a failed-budget mutation")
		}
	}
}

func TestProviderBudgetRecoveryCannotBypassAdmission(t *testing.T) {
	for _, tc := range []struct {
		name      string
		recovered bool
		summary   string
		err       error
	}{
		{name: "declined"},
		{name: "still oversized", recovered: true},
		{name: "summary longer than the history budget", recovered: true, summary: strings.Repeat("s", 6000)},
		{name: "compaction failed", err: fmt.Errorf("%w: summarizer down", contextfrag.ErrBudgetUnsatisfied)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := recoveryBudgetFixture()
			calls := 0
			cfg.RecoverContextBudget = func(_ context.Context, cfg native.RunConfig) (native.RunConfig, bool, error) {
				calls++
				if tc.summary != "" {
					summary := stableHistoryMessageFrag("summary", sdk.UserMessage(tc.summary))
					summary.Kind = contextfrag.KindConversationSummary
					cfg.ContextSourceFrags = append([]contextfrag.ContextFrag(nil), cfg.ContextSourceFrags...)
					cfg.ContextSourceFrags[1] = summary
				}
				return cfg, tc.recovered, tc.err
			}
			got, err := ProviderRunConfigApplier(nil)(context.Background(), cfg)
			if !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) || calls != 1 {
				t.Fatalf("recovery calls=%d error=%v, want ErrBudgetUnsatisfied", calls, err)
			}
			found := false
			for _, mutation := range got.ContextMutations.Records() {
				found = found || mutation.Kind == contextfrag.MutationContextBudgetFailure
			}
			if !found {
				t.Fatal("unresolved overflow must retain failed-budget evidence")
			}
		})
	}
}

func TestProviderBudgetRecoveryIgnoresUnrecoverableRequests(t *testing.T) {
	for _, name := range []string{"fixed overhead", "canceled"} {
		t.Run(name, func(t *testing.T) {
			cfg := recoveryBudgetFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch name {
			case "fixed overhead":
				cfg.ContextToolDefs[0].TokenEstimate = 16000
			case "canceled":
				cancel()
			}
			cfg.RecoverContextBudget = func(context.Context, native.RunConfig) (native.RunConfig, bool, error) {
				t.Fatal("request cannot be rescued by compacting history")
				return native.RunConfig{}, false, nil
			}
			if _, err := ProviderRunConfigApplier(nil)(ctx, cfg); err == nil {
				t.Fatal("unrecoverable request was admitted")
			}
		})
	}
}

func TestProviderBudgetRecoveryFailureKeepsBudgetAudit(t *testing.T) {
	cfg := recoveryBudgetFixture()
	cfg.RecoverContextBudget = func(context.Context, native.RunConfig) (native.RunConfig, bool, error) {
		return native.RunConfig{}, false, errors.New("private compaction failure")
	}
	got, err := ProviderRunConfigApplier(nil)(context.Background(), cfg)
	if !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) || got.ContextManifest.BudgetPlan == nil {
		t.Fatalf("failed recovery lost budget error or plan: %v / %+v", err, got.ContextManifest.BudgetPlan)
	}
	if len(got.ContextMutations.Records()) == 0 {
		t.Fatal("failed recovery lost budget audit")
	}
}

func TestProviderBudgetRecoveryRecomposeDoesNotRecordBudgetFailure(t *testing.T) {
	cfg := recoveryBudgetFixture()
	cfg.RecoverContextBudget = func(_ context.Context, cfg native.RunConfig) (native.RunConfig, bool, error) {
		return cfg, false, native.ErrContextRecompose
	}
	got, err := ProviderRunConfigApplier(nil)(context.Background(), cfg)
	if !errors.Is(err, native.ErrContextRecompose) {
		t.Fatalf("recompose control = %v", err)
	}
	if got.ContextMutations != nil {
		for _, mutation := range got.ContextMutations.Records() {
			if mutation.Kind == contextfrag.MutationContextBudgetFailure {
				t.Fatal("successful compaction recompose is not a budget failure")
			}
		}
	}
}

func TestProviderBudgetRecoveryHonorsCancellation(t *testing.T) {
	cfg := recoveryBudgetFixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg.RecoverContextBudget = func(_ context.Context, cfg native.RunConfig) (native.RunConfig, bool, error) {
		cancel()
		return cfg, true, nil
	}
	if _, err := ProviderRunConfigApplier(nil)(ctx, cfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("recovery cancellation = %v", err)
	}
}

// An armed run over its budget does not fail before it dispatches: it folds
// what does not fit at the step, against the plan selection leaves it. A
// tool exchange missing its other half is still cut.
func TestProviderArmedRunKeepsItsHistory(t *testing.T) {
	cfg := recoveryBudgetFixture()
	orphan := stableHistoryMessageFrag("orphan", sdk.ToolMessage(sdk.ToolResultPart{ToolCallID: "gone", ToolName: "exec", Result: toolexec.OutputFromValue("lost")}))
	cfg.ContextSourceFrags = append(cfg.ContextSourceFrags[:2], orphan, cfg.ContextSourceFrags[2])
	cfg.SwitchContext = func(context.Context, native.ContextSwitch) (string, error) { return "", nil }
	got, err := ProviderRunConfigApplier(nil)(t.Context(), cfg)
	if err != nil {
		t.Fatalf("an armed run over its budget failed before folding: %v", err)
	}
	want := []sdk.Message{sdk.AssistantMessage(strings.Repeat("h", 8000)), sdk.UserMessage("continue")}
	if !reflect.DeepEqual(got.Messages, want) {
		t.Fatalf("messages = %+v, want the whole history without the orphan result", got.Messages)
	}
	if plan := got.ContextManifest.BudgetPlan; plan == nil || plan.HistoryBudget != 998 {
		t.Fatalf("budget plan = %+v, want the plan the fold sizes against", plan)
	}
}

// A run that cannot fold fails when its history does not fit, and its audit
// still holds the whole history: nothing was trimmed to make it fit.
func TestProviderUnarmedRunOverItsBudgetFails(t *testing.T) {
	got, err := ProviderRunConfigApplier(nil)(t.Context(), recoveryBudgetFixture())
	if !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) {
		t.Fatalf("an unarmed run over its budget = %v, want ErrBudgetUnsatisfied", err)
	}
	if ids := fragIDs(got.ContextFrags); !reflect.DeepEqual(ids, []string{"system", "history", "current"}) {
		t.Fatalf("audit = %v, want the whole history", ids)
	}
	records := got.ContextMutations.Records()
	if len(records) != 1 || records[0] != (contextfrag.MutationRecord{
		Kind: contextfrag.MutationContextBudgetFailure, Detail: "budget_unsatisfied",
	}) {
		t.Fatalf("mutations = %#v, want one budget_unsatisfied record", records)
	}
}
