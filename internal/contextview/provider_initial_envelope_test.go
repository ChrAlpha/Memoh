package contextview

import (
	"context"
	"errors"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	agentpkg "github.com/felinics/memoh/internal/agent/runtime/native"
)

// The rendered-envelope check prices what renders, message by message: a
// fragment that renders as many tiny messages pays for each of them, and a
// run that cannot fold what does not fit fails closed before dispatch.
func TestApplyProviderRunConfigFailsClosedWhenRenderingOutgrowsItsFragments(t *testing.T) {
	t.Parallel()

	tiny := sdk.UserMessage("x")
	burst := historyMessageFrag("burst", tiny)
	for range 39 {
		burst.Parts = append(burst.Parts, contextfrag.Part{Type: contextfrag.PartSDKMessage, SDKMessage: &tiny})
	}
	frags := []contextfrag.ContextFrag{
		systemTextFrag("system", strings.Repeat("s", 2_784), contextfrag.KindSystemPrompt, 100),
		burst,
		currentMessageFrag("current", "current"),
	}
	out, err := ProviderRunConfigApplier(nil)(context.Background(), agentpkg.RunConfig{
		ContextSourceFrags:     frags,
		ContextBudgetMaxTokens: 1_200,
	})
	if !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) {
		t.Fatalf("preflight error = %v, want rendered-envelope ErrBudgetUnsatisfied", err)
	}
	if !strings.Contains(err.Error(), "rendered_input=") {
		t.Fatalf("preflight error = %v, want the rendered-envelope detail", err)
	}
	records := out.ContextMutations.Records()
	if len(records) != 1 || records[0] != (contextfrag.MutationRecord{
		Kind: contextfrag.MutationContextBudgetFailure, Detail: "budget_unsatisfied",
	}) {
		t.Fatalf("rendered-envelope mutations = %#v, want one budget_unsatisfied record", records)
	}
}
