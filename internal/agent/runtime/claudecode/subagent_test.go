package claudecode

import (
	"context"
	"slices"
	"testing"

	"github.com/felinics/memoh/internal/agent/runtime/claudecode/claudecfg"
	"github.com/felinics/memoh/internal/agent/runtime/external"
)

func TestChildPolicyDisablesDelegationAndQuestionsWithoutChangingApprovalMode(t *testing.T) {
	input := external.PromptInput{SessionMode: "subagent", CanRequestUserInput: true}
	args := cliArgs(claudecfg.Config{PermissionMode: "default"}, input, "", "", "2.1.0")
	for _, name := range []string{"--disallowedTools", "Agent", "Task", "TeamCreate", "TeamDelete", "SendMessage", "AskUserQuestion"} {
		if !slices.Contains(args, name) {
			t.Fatalf("missing %s: %v", name, args)
		}
	}
	if input.CanAskUser() || slices.Contains(args, "--allow-dangerously-skip-permissions") {
		t.Fatal("child bypassed question or approval policy")
	}
	r := newTestRunner(&recordingSink{})
	defer r.close()
	r.input = input
	if _, ok := r.collectAnswers(context.Background(), "request", &controlRequestPayload{ToolName: "AskUserQuestion", ToolUseID: "ask", Input: map[string]any{}}); ok {
		t.Fatal("child asked its parent user directly")
	}
	parent := cliArgs(claudecfg.Config{}, external.PromptInput{}, "", "", "")
	if slices.Contains(parent, "--disallowedTools") {
		t.Fatal("child restrictions leaked into parent")
	}
}
