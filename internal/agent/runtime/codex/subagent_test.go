package codex

import (
	"context"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/runtime/agentprocess"
	"github.com/felinics/memoh/internal/agent/runtime/external"
)

func TestChildServerScopeAndCommandDoNotReuseParentOrSibling(t *testing.T) {
	parent := external.PromptInput{BotID: "bot", BotAgentID: "instance", ThreadID: "parent", RunID: "run-parent"}
	child := parent
	child.SessionMode = "subagent"
	child.ThreadID = "child"
	child.RunID = "run-child"
	sibling := child
	sibling.RunID = "run-sibling"
	if promptServerKey(parent) == promptServerKey(child) || promptServerKey(child) == promptServerKey(sibling) {
		t.Fatal("execution server scopes collide")
	}
	scope := agentprocess.ExecutionScopeFromContext(scopedServerContext(context.Background(), promptServerKey(child)))
	if scope.ThreadID != child.ThreadID || scope.RunID != child.RunID {
		t.Fatalf("lost process scope: %+v", scope)
	}
	command := appServerCommand("/data/codex", true)
	if !strings.Contains(command, "features.multi_agent=false") || !strings.Contains(command, "features.goals=false") || strings.Contains(command, "=true") {
		t.Fatal(command)
	}
	if !strings.Contains(appServerCommand("/data/codex", false), "features.goals=true") {
		t.Fatal("parent policy changed")
	}
}
