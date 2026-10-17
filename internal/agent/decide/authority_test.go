package decide

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/agenttest"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	"github.com/felinics/memoh/internal/bots"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// runInitiatorQueries serves session_runs.initiator_id by run id.
type runInitiatorQueries struct {
	dbstore.Queries
	initiators map[string]string
}

func (q runInitiatorQueries) GetSessionRun(_ context.Context, id pgtype.UUID) (sqlc.SessionRun, error) {
	initiator, ok := q.initiators[id.String()]
	if !ok {
		return sqlc.SessionRun{}, pgx.ErrNoRows
	}
	return sqlc.SessionRun{RunID: id, InitiatorID: pgtype.Text{String: initiator, Valid: initiator != ""}}, nil
}

const (
	approvalTestRunID       = "11111111-1111-4111-8111-111111111111"
	approvalTestLegacyRunID = "22222222-2222-4222-8222-222222222222"
	approvalTestInitiator   = "initiator-user"
	approvalTestManager     = "manager-user"
	approvalTestGrantHolder = "grant-holder-user"
	approvalTestOutsider    = "outsider-user"
)

func approvalAuthorityService(grants map[string][]string) *Decider {
	values := map[string]bool{}
	for actor, permissions := range grants {
		for _, permission := range permissions {
			values["bot-1:"+actor+":"+permission] = true
		}
	}
	return New(Deps{
		BotPermissions: &agenttest.PermissionChecker{Values: values},
		Queries: runInitiatorQueries{initiators: map[string]string{
			approvalTestRunID:       approvalTestInitiator,
			approvalTestLegacyRunID: "",
		}},
	})
}

func TestCanDecideToolApprovalAdmitsInitiatorAndManagerOnly(t *testing.T) {
	t.Parallel()

	svc := approvalAuthorityService(map[string][]string{
		approvalTestInitiator:   {bots.PermissionWorkspaceExec},
		approvalTestManager:     {bots.PermissionManage},
		approvalTestGrantHolder: {bots.PermissionWorkspaceExec},
		approvalTestOutsider:    {bots.PermissionChat},
	})
	exec := toolapproval.Request{BotID: "bot-1", RunID: approvalTestRunID, Operation: toolapproval.OperationExec}
	legacy := toolapproval.Request{BotID: "bot-1", RunID: approvalTestLegacyRunID, Operation: toolapproval.OperationExec}
	unfenced := toolapproval.Request{BotID: "bot-1", Operation: toolapproval.OperationExec}
	cases := []struct {
		name     string
		approval toolapproval.Request
		actor    string
		want     bool
	}{
		{"initiator", exec, approvalTestInitiator, true},
		{"manager", exec, approvalTestManager, true},
		{"grant holder on another user's run", exec, approvalTestGrantHolder, false},
		{"outsider", exec, approvalTestOutsider, false},
		{"run without initiator: unrecorded requester", legacy, approvalTestInitiator, false},
		{"run without initiator: manager", legacy, approvalTestManager, true},
		{"approval outside a run: grant holder", unfenced, approvalTestGrantHolder, false},
		{"approval outside a run: manager", unfenced, approvalTestManager, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.CanDecideToolApproval(context.Background(), tc.approval, ToolApprovalPrincipal{UserID: tc.actor})
			if err != nil || got != tc.want {
				t.Fatalf("CanDecideToolApproval(%s) = %t, %v; want %t", tc.actor, got, err, tc.want)
			}
			// The write path is the same function: a submission is refused
			// exactly when the viewer is not offered the decision.
			err = svc.authorizeToolApprovalResponse(context.Background(), tc.approval, ToolApprovalResponseInput{ActorUserID: tc.actor})
			if tc.want && err != nil {
				t.Fatalf("authorize(%s) error = %v, want allowed", tc.actor, err)
			}
			if !tc.want && !errors.Is(err, toolapproval.ErrForbidden) {
				t.Fatalf("authorize(%s) error = %v, want forbidden", tc.actor, err)
			}
		})
	}
}

// The initiator keeps the authority the operation itself requires: starting
// a run does not lend a chat-only user the right to approve workspace changes.
func TestCanDecideToolApprovalInitiatorNeedsOperationGrant(t *testing.T) {
	t.Parallel()

	cases := []struct {
		operation  string
		permission string
	}{
		{operation: toolapproval.OperationRead, permission: bots.PermissionWorkspaceRead},
		{operation: toolapproval.OperationWrite, permission: bots.PermissionWorkspaceWrite},
		{operation: toolapproval.OperationExec, permission: bots.PermissionWorkspaceExec},
		{operation: toolapproval.OperationPermission, permission: bots.PermissionWorkspaceExec},
	}
	for _, tc := range cases {
		t.Run(tc.operation, func(t *testing.T) {
			t.Parallel()
			approval := toolapproval.Request{BotID: "bot-1", RunID: approvalTestRunID, Operation: tc.operation}
			principal := ToolApprovalPrincipal{UserID: approvalTestInitiator}
			granted := approvalAuthorityService(map[string][]string{approvalTestInitiator: {tc.permission}})
			if ok, err := granted.CanDecideToolApproval(context.Background(), approval, principal); err != nil || !ok {
				t.Fatalf("initiator holding %s = %t, %v; want allowed", tc.permission, ok, err)
			}
			chatOnly := approvalAuthorityService(map[string][]string{approvalTestInitiator: {bots.PermissionChat}})
			if ok, err := chatOnly.CanDecideToolApproval(context.Background(), approval, principal); err != nil || ok {
				t.Fatalf("chat-only initiator = %t, %v; want refused", ok, err)
			}
		})
	}
}

func TestCanDecideToolApprovalMatchesInitiatorByChannelIdentity(t *testing.T) {
	t.Parallel()

	// An IM requester without a bound account was recorded by its channel
	// identity, and its grants are keyed on that identity too.
	svc := approvalAuthorityService(map[string][]string{approvalTestInitiator: {bots.PermissionWorkspaceExec}})
	approval := toolapproval.Request{BotID: "bot-1", RunID: approvalTestRunID, Operation: toolapproval.OperationExec}
	if ok, err := svc.CanDecideToolApproval(context.Background(), approval, ToolApprovalPrincipal{ChannelIdentityID: approvalTestInitiator}); err != nil || !ok {
		t.Fatalf("channel-identity initiator = %t, %v; want allowed", ok, err)
	}
}

func TestCanDecideToolApprovalFailsClosed(t *testing.T) {
	t.Parallel()

	checker := &agenttest.RecordingPermissionChecker{Allow: true}
	resolver := New(Deps{BotPermissions: checker})
	for _, tc := range []struct {
		approval  toolapproval.Request
		principal ToolApprovalPrincipal
	}{
		{toolapproval.Request{Operation: toolapproval.OperationRead}, ToolApprovalPrincipal{UserID: "user-1"}},
		{toolapproval.Request{BotID: "bot-1", Operation: toolapproval.OperationRead}, ToolApprovalPrincipal{}},
	} {
		if ok, err := resolver.CanDecideToolApproval(context.Background(), tc.approval, tc.principal); ok || err != nil {
			t.Fatalf("CanDecideToolApproval(%+v, %+v) = %t, %v; want refused", tc.approval, tc.principal, ok, err)
		}
	}
	// A run id cannot be resolved without the run store.
	if _, err := resolver.CanDecideToolApproval(context.Background(), toolapproval.Request{BotID: "bot-1", RunID: approvalTestRunID}, ToolApprovalPrincipal{UserID: "user-1"}); err == nil {
		t.Fatal("unresolvable initiator was treated as absent")
	}
	if ok, err := New(Deps{}).CanDecideToolApproval(context.Background(), toolapproval.Request{BotID: "bot-1"}, ToolApprovalPrincipal{UserID: "user-1"}); ok || err == nil {
		t.Fatalf("missing permission checker = %t, %v; want error", ok, err)
	}
}

func TestCapabilityApprovalRequiresManage(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"mcp_manage", "app_manage"} {
		t.Run(name, func(t *testing.T) {
			checker := &agenttest.RecordingPermissionChecker{Allow: false}
			d := New(Deps{BotPermissions: checker})
			target := toolapproval.Request{ID: "approval-1", BotID: "bot-1", ToolName: name, Operation: toolapproval.OperationPermission, Status: toolapproval.StatusApproved}
			input := ToolApprovalResponseInput{ActorUserID: "user-1"}
			if err := d.authorizeToolApprovalResponse(context.Background(), target, input); !errors.Is(err, toolapproval.ErrForbidden) || checker.Permission != bots.PermissionManage {
				t.Fatalf("management approval permission = %q, error = %v", checker.Permission, err)
			}
			checker.Allow = true
			if err := d.authorizeToolApprovalResponse(context.Background(), target, input); err != nil {
				t.Fatal(err)
			}
		})
	}
}
