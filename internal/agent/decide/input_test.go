package decide

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/bots"
	session "github.com/felinics/memoh/internal/chat/thread"
)

func TestUserInputAnswersFromText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		payload    userinput.UIPayload
		text       string
		wantAnswer userinput.QuestionAnswer
		wantErr    bool
	}{
		{
			name: "text question",
			payload: userinput.UIPayload{Questions: []userinput.UIQuestion{{
				ID:   "q1",
				Kind: userinput.QuestionKindText,
			}}},
			text:       "ship it",
			wantAnswer: userinput.QuestionAnswer{QuestionID: "q1", Text: "ship it"},
		},
		{
			name: "single select by label",
			payload: userinput.UIPayload{Questions: []userinput.UIQuestion{{
				ID:   "q1",
				Kind: userinput.QuestionKindSingleSelect,
				Options: []userinput.UIOption{
					{ID: "q1.o1", Label: "Plan A"},
					{ID: "q1.o2", Label: "Plan B"},
				},
			}}},
			text:       "plan b",
			wantAnswer: userinput.QuestionAnswer{QuestionID: "q1", OptionIDs: []string{"q1.o2"}},
		},
		{
			name: "multi select by labels",
			payload: userinput.UIPayload{Questions: []userinput.UIQuestion{{
				ID:   "q1",
				Kind: userinput.QuestionKindMultiSelect,
				Options: []userinput.UIOption{
					{ID: "q1.o1", Label: "One"},
					{ID: "q1.o2", Label: "Two"},
				},
			}}},
			text:       "One, Two",
			wantAnswer: userinput.QuestionAnswer{QuestionID: "q1", OptionIDs: []string{"q1.o1", "q1.o2"}},
		},
		{
			name: "custom select answer",
			payload: userinput.UIPayload{Questions: []userinput.UIQuestion{{
				ID:          "q1",
				Kind:        userinput.QuestionKindSingleSelect,
				AllowCustom: true,
				Options:     []userinput.UIOption{{ID: "q1.o1", Label: "Known"}},
			}}},
			text:       "Something else",
			wantAnswer: userinput.QuestionAnswer{QuestionID: "q1", CustomText: "Something else"},
		},
		{
			name: "multiple questions unsupported",
			payload: userinput.UIPayload{Questions: []userinput.UIQuestion{
				{ID: "q1", Kind: userinput.QuestionKindText},
				{ID: "q2", Kind: userinput.QuestionKindText},
			}},
			text:    "answer",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := userInputAnswersFromText(tt.payload, tt.text)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("userInputAnswersFromText() error = %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("answers = %#v, want one", got)
			}
			if got[0].QuestionID != tt.wantAnswer.QuestionID || got[0].Text != tt.wantAnswer.Text || got[0].CustomText != tt.wantAnswer.CustomText {
				t.Fatalf("answer = %#v, want %#v", got[0], tt.wantAnswer)
			}
			if strings.Join(got[0].OptionIDs, ",") != strings.Join(tt.wantAnswer.OptionIDs, ",") {
				t.Fatalf("option ids = %#v, want %#v", got[0].OptionIDs, tt.wantAnswer.OptionIDs)
			}
		})
	}
}

// TestACPDecisionAuthorityRequiresLiveWorkspaceExec pins the decision-time
// authority model: the runtime owner has no standing beyond their live grants,
// so a revoked or offboarded owner loses approval/response authority, while
// any member holding workspace_exec (the disclosed widened semantics) keeps it.
func TestACPDecisionAuthorityRequiresLiveWorkspaceExec(t *testing.T) {
	t.Parallel()

	const (
		ownerID    = "owner-user"
		memberID   = "member-user"
		chatOnlyID = "chat-only-user"
	)
	perms := &agenttest.PermissionChecker{
		Values: map[string]bool{
			"bot-1:" + ownerID + ":" + bots.PermissionWorkspaceExec:  true,
			"bot-1:" + memberID + ":" + bots.PermissionWorkspaceExec: true,
			"bot-1:" + chatOnlyID + ":" + bots.PermissionChat:        true,
		},
	}
	svc := New(Deps{
		BotPermissions: perms,
		Sessions: &agenttest.Sessions{
			GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
				return session.Thread{
					ID:          sessionID,
					BotID:       "bot-1",
					Type:        session.TypeACPAgent,
					RuntimeType: session.RuntimeACPAgent,
					RuntimeMetadata: map[string]any{
						"runtime_owner_account_id": ownerID,
					},
				}, nil
			},
		},
	})
	inputTarget := userinput.Request{BotID: "bot-1", SessionID: "session-1"}
	svc.queries = runInitiatorQueries{initiators: map[string]string{approvalTestRunID: ownerID}}
	approvalTarget := toolapproval.Request{BotID: "bot-1", SessionID: "session-1", RunID: approvalTestRunID, Operation: toolapproval.OperationExec}

	if err := svc.authorizeExternalAgentUserInputResponse(context.Background(), inputTarget, UserInputResponseInput{ActorUserID: ownerID}); err != nil {
		t.Fatalf("owner user-input authorization error = %v", err)
	}
	if err := svc.authorizeExternalAgentToolApprovalResponse(context.Background(), approvalTarget, ToolApprovalResponseInput{ActorUserID: ownerID}); err != nil {
		t.Fatalf("owner approval authorization error = %v", err)
	}
	if err := svc.authorizeExternalAgentUserInputResponse(context.Background(), inputTarget, UserInputResponseInput{ActorUserID: memberID}); err != nil {
		t.Fatalf("workspace_exec member user-input authorization error = %v", err)
	}
	// The member's grant does not reach a run the owner started.
	if err := svc.authorizeExternalAgentToolApprovalResponse(context.Background(), approvalTarget, ToolApprovalResponseInput{ActorUserID: memberID}); !errors.Is(err, toolapproval.ErrForbidden) {
		t.Fatalf("workspace_exec member approval authorization error = %v, want forbidden", err)
	}
	if err := svc.authorizeExternalAgentUserInputResponse(context.Background(), inputTarget, UserInputResponseInput{ActorUserID: chatOnlyID}); !errors.Is(err, userinput.ErrForbidden) {
		t.Fatalf("chat-only user-input authorization error = %v, want forbidden", err)
	}
	if err := svc.authorizeExternalAgentToolApprovalResponse(context.Background(), approvalTarget, ToolApprovalResponseInput{ActorUserID: chatOnlyID}); !errors.Is(err, toolapproval.ErrForbidden) {
		t.Fatalf("chat-only approval authorization error = %v, want forbidden", err)
	}

	delete(perms.Values, "bot-1:"+ownerID+":"+bots.PermissionWorkspaceExec)
	if err := svc.authorizeExternalAgentUserInputResponse(context.Background(), inputTarget, UserInputResponseInput{ActorUserID: ownerID}); !errors.Is(err, userinput.ErrForbidden) {
		t.Fatalf("revoked owner user-input authorization error = %v, want forbidden", err)
	}
	if err := svc.authorizeExternalAgentToolApprovalResponse(context.Background(), approvalTarget, ToolApprovalResponseInput{ActorUserID: ownerID}); !errors.Is(err, toolapproval.ErrForbidden) {
		t.Fatalf("revoked owner approval authorization error = %v, want forbidden", err)
	}
}
