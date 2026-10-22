package decide

import (
	"testing"

	"github.com/felinics/memoh/internal/agent/agentapi"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/toolexec"
)

func approvalCall(t *testing.T, status, reason string) DecidedCall {
	t.Helper()
	request := toolapproval.Request{
		ID: "approval-1", BotID: "bot-1", SessionID: "session-1", ToolCallID: "call-1", ToolName: "exec",
		ToolInput: map[string]any{"command": "ls"}, Status: status, ChannelIdentityID: "identity-1",
		SourcePlatform: "web", ConversationType: "private", WorkspaceTargetID: "target-1",
	}
	input := agentapi.ToolApprovalResponseInput{BotID: "bot-1", ChatToken: "token", ActorChannelIdentityID: "actor-1", Reason: reason}
	handle := sessionruntime.RunHandle{RunID: "run-1", TurnID: "turn-1"}
	return newTestDecider(Deps{}).ToolApprovalCall(NewCommittedToolApproval(request, input, false).ForRun("run-1", handle))
}

func TestToolApprovalCallApprovedHasNoResultYet(t *testing.T) {
	t.Parallel()

	call := approvalCall(t, toolapproval.StatusApproved, "")
	if call.Verdict != VerdictApproved || call.Result.ToolCallID != "" {
		t.Fatalf("call = %#v, want an approved call with no result", call)
	}
	if call.Kind != CallToolApproval || call.DecisionID != "approval-1" || call.ToolCallID != "call-1" || call.ToolName != "exec" {
		t.Fatalf("call identity = %#v", call)
	}
	if got, _ := call.ToolInput.(map[string]any); got["command"] != "ls" {
		t.Fatalf("tool input = %#v, want the call's arguments", call.ToolInput)
	}
	if call.RunID != "run-1" || call.RunHandle.TurnID != "turn-1" || call.BotID != "bot-1" || call.ChatToken != "token" || call.WorkspaceTargetID != "target-1" {
		t.Fatalf("call route = %#v", call)
	}
	if call.ChannelIdentityID != "identity-1" {
		t.Fatalf("channel identity = %q, want the decision row's before the actor's", call.ChannelIdentityID)
	}
	if call.SkipMemoryExtraction {
		t.Fatal("an approval resumes with memory extraction")
	}
	if err := call.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestToolApprovalCallRejectedCarriesTheRefusalAsAnErrorResult(t *testing.T) {
	t.Parallel()

	for reason, want := range map[string]string{
		"":     "tool execution rejected by user",
		"nope": "tool execution rejected by user: nope",
	} {
		call := approvalCall(t, toolapproval.StatusRejected, reason)
		if call.Verdict != VerdictRejected || !call.Result.IsError || call.Result.ToolCallID != "call-1" || call.Result.ToolName != "exec" {
			t.Fatalf("call = %#v, want a rejected call whose result is an error", call)
		}
		if got := toolexec.OutputValue(call.Result.Result); got != want {
			t.Fatalf("rejection result = %#v, want %q", got, want)
		}
	}
}

func TestToolApprovalCallFallsBackToTheActorWhenTheRowNamesNoIdentity(t *testing.T) {
	t.Parallel()

	request := toolapproval.Request{ID: "approval-1", Status: toolapproval.StatusApproved}
	input := agentapi.ToolApprovalResponseInput{BotID: "bot-1", ActorChannelIdentityID: "actor-1"}
	call := newTestDecider(Deps{}).ToolApprovalCall(NewCommittedToolApproval(request, input, false))
	if call.ChannelIdentityID != "actor-1" || call.ScopeBotID != "bot-1" {
		t.Fatalf("call = identity:%q scope bot:%q, want the actor and the response's bot", call.ChannelIdentityID, call.ScopeBotID)
	}
}

func TestToolApprovalCallScopesTheSummaryToTheDecisionRowsBot(t *testing.T) {
	t.Parallel()

	request := toolapproval.Request{ID: "approval-1", BotID: "row-bot", Status: toolapproval.StatusApproved}
	call := newTestDecider(Deps{}).ToolApprovalCall(NewCommittedToolApproval(request, agentapi.ToolApprovalResponseInput{BotID: "input-bot"}, false))
	if call.ScopeBotID != "row-bot" || call.BotID != "input-bot" {
		t.Fatalf("bots = scope:%q response:%q, want the row's scope and the response's bot", call.ScopeBotID, call.BotID)
	}
	answer := userinput.Request{ID: "input-1", BotID: "row-bot"}
	got := newTestDecider(Deps{}).UserInputCall(NewCommittedUserInput(answer, agentapi.UserInputResponseInput{BotID: "input-bot"}))
	if got.ScopeBotID != "row-bot" || got.BotID != "input-bot" {
		t.Fatalf("bots = scope:%q response:%q, want the row's scope and the response's bot", got.ScopeBotID, got.BotID)
	}
}

func TestDecidedCallValidateNamesWhatIsWrong(t *testing.T) {
	t.Parallel()

	if got := approvalCall(t, "expired", "").Validate(); got == nil || got.Error() != `committed tool approval has unexpected status "expired"` {
		t.Fatalf("Validate() = %v, want the unexpected status", got)
	}
	approval := newTestDecider(Deps{}).ToolApprovalCall(NewCommittedToolApproval(toolapproval.Request{Status: toolapproval.StatusApproved}, agentapi.ToolApprovalResponseInput{}, false))
	if got := approval.Validate(); got == nil || got.Error() != "committed tool approval response is missing its request" {
		t.Fatalf("Validate() = %v, want the missing approval", got)
	}
	answer := newTestDecider(Deps{}).UserInputCall(NewCommittedUserInput(userinput.Request{}, agentapi.UserInputResponseInput{}))
	if got := answer.Validate(); got == nil || got.Error() != "committed user input response is missing its request" {
		t.Fatalf("Validate() = %v, want the missing question", got)
	}
}

func TestUserInputCallIsAnAnswerThatSchedulesNoMemoryExtraction(t *testing.T) {
	t.Parallel()

	request := userinput.Request{
		ID: "input-1", SessionID: "session-1", ToolCallID: "call-1", ToolName: userinput.ToolNameAskUser,
		Status: userinput.StatusSubmitted, Result: map[string]any{"status": userinput.StatusSubmitted},
		SourcePlatform: "web", WorkspaceTargetID: "target-1",
	}
	call := newTestDecider(Deps{}).UserInputCall(NewCommittedUserInput(request, agentapi.UserInputResponseInput{BotID: "bot-1", ChatToken: "token"}))
	if call.Kind != CallUserInput || call.Verdict != VerdictAnswered || !call.SkipMemoryExtraction {
		t.Fatalf("call = %#v, want an answered question that skips memory extraction", call)
	}
	if call.Result.IsError || call.Result.ToolCallID != "call-1" || call.Result.ToolName != userinput.ToolNameAskUser {
		t.Fatalf("answer result = %#v, want a successful result of the ask_user call", call.Result)
	}
	if call.ReplyTarget != "" {
		// The request names no bot, so a web reply target has nothing to fall
		// back to.
		t.Fatalf("reply target = %q", call.ReplyTarget)
	}
	request.BotID = "bot-1"
	web := newTestDecider(Deps{}).UserInputCall(NewCommittedUserInput(request, agentapi.UserInputResponseInput{BotID: "bot-1"}))
	if web.ReplyTarget != "bot-1" {
		t.Fatalf("web reply target = %q, want the bot's", web.ReplyTarget)
	}
}
