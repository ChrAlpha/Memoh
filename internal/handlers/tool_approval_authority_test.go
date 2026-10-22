package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/felinics/memoh/internal/accounts"
	"github.com/felinics/memoh/internal/agent/agentapi"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	chatview "github.com/felinics/memoh/internal/agent/view"
	"github.com/felinics/memoh/internal/auth"
	"github.com/felinics/memoh/internal/bots"
	"github.com/felinics/memoh/internal/channel"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/server"
)

const (
	approvalEntryBotID       = "11111111-1111-1111-1111-111111111111"
	approvalEntrySessionID   = "22222222-2222-2222-2222-222222222222"
	approvalEntryRunID       = "33333333-3333-3333-3333-333333333333"
	approvalEntryDecisionID  = "44444444-4444-4444-4444-444444444444"
	approvalEntryCreator     = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	approvalEntryInitiator   = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	approvalEntryGrantHolder = "cccccccc-cccc-cccc-cccc-cccccccccccc"
)

// approvalEntryVerdict stands in for CanDecideToolApproval at the owner: only
// the run's initiator may decide. Each entry must hand it the actor and
// surface its answer unchanged.
func approvalEntryVerdict(userID, channelIdentityID string) error {
	if userID == approvalEntryInitiator || channelIdentityID == approvalEntryInitiator {
		return nil
	}
	return toolapproval.ErrForbidden
}

// verdictDecisionRuntime is the routed owner: it decodes the decision the WS
// entry forwarded and answers with the verdict.
type verdictDecisionRuntime struct {
	testWSDecisionRuntime
}

func (*verdictDecisionRuntime) RouteDecisionResponse(_ context.Context, response sessionruntime.DecisionResponse) (sessionruntime.DecisionResponseResult, error) {
	var input agentapi.ToolApprovalResponseInput
	if err := json.Unmarshal(response.Payload, &input); err != nil {
		return sessionruntime.DecisionResponseResult{}, err
	}
	if err := approvalEntryVerdict(input.ActorUserID, input.ActorChannelIdentityID); err != nil {
		return sessionruntime.DecisionResponseResult{Handled: true}, err
	}
	return sessionruntime.DecisionResponseResult{Handled: true, Applied: true}, nil
}

// The WS entry adds no check of its own: the initiator of a run in a session
// somebody else created holds no session access, and still decides, because
// the owner's verdict is the only one.
func TestToolApprovalWSEntryAnswersWithTheOwnerVerdict(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		actor    string
		applied  bool
		wantCode string
	}{
		{actor: approvalEntryInitiator, applied: true},
		{actor: approvalEntryGrantHolder, wantCode: "tool_approval.forbidden"},
	} {
		t.Run(tc.actor, func(t *testing.T) {
			t.Parallel()
			queries := localChannelSessionAuthQueries{
				bot: testBotRow(approvalEntryBotID, map[string]any{}),
				session: sqlc.BotSession{
					ID: testUUID(approvalEntrySessionID), BotID: testUUID(approvalEntryBotID),
					Type: sessionpkg.TypeChat, SessionMode: sessionpkg.TypeChat,
					CreatedByUserID: testUUID(approvalEntryCreator), Metadata: []byte(`{}`),
				},
				grants: []sqlc.ListBotUserGrantsForUserRow{{
					ID: testUUID("dddddddd-dddd-dddd-dddd-dddddddddddd"), BotID: testUUID(approvalEntryBotID),
					SubjectType: bots.GrantSubjectUser, UserID: testUUID(tc.actor),
					Permissions: []byte(`["chat","workspace_exec"]`),
				}},
			}
			handler := &LocalChannelHandler{
				channelType:    channel.ChannelTypeLocal,
				botService:     bots.NewService(nil, queries),
				accountService: accounts.NewService(nil, testAdminAccountStore{role: "user"}),
				sessionService: sessionpkg.NewService(nil, queries, nil),
				sessionRuntime: &verdictDecisionRuntime{},
				webRuns:        inertAgent{}, queue: inertAgent{}, runtimeOps: inertAgent{}, decisions: inertAgent{},
				logger: slog.Default(),
			}
			client := openLocalChannelTestWS(t, handler, approvalEntryBotID, tc.actor)
			if err := client.WriteJSON(map[string]any{
				"type": "tool_approval_response", "session_id": approvalEntrySessionID, "run_id": approvalEntryRunID,
				"decision_id": approvalEntryDecisionID, "control_id": "control-approval", "decision": "approve",
			}); err != nil {
				t.Fatal(err)
			}
			if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			var ack wsOutboundEvent
			if err := client.ReadJSON(&ack); err != nil {
				t.Fatal(err)
			}
			if ack.Type != "control_ack" || ack.Applied != tc.applied || ack.Code != tc.wantCode {
				t.Fatalf("ack = %+v, want applied=%t code=%q", ack, tc.applied, tc.wantCode)
			}
		})
	}
}

type verdictToolApprovalResponder struct{}

func (verdictToolApprovalResponder) RespondToolApproval(_ context.Context, input turn.ToolApprovalResponse, _ chan<- json.RawMessage) error {
	return approvalEntryVerdict(input.ActorUserID, input.ActorChannelIdentityID)
}

func TestToolApprovalHTTPEntryAnswersWithTheServiceVerdict(t *testing.T) {
	t.Parallel()

	const secret = "test-secret"
	for _, tc := range []struct {
		actor    string
		status   int
		wantCode string
	}{
		{actor: approvalEntryInitiator, status: http.StatusOK},
		{actor: approvalEntryGrantHolder, status: http.StatusForbidden, wantCode: "tool_approval.forbidden"},
	} {
		t.Run(tc.actor, func(t *testing.T) {
			t.Parallel()
			token, _, err := auth.GenerateChatToken(auth.ChatToken{BotID: approvalEntryBotID, ChatID: "chat-1", UserID: tc.actor}, secret, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			e := echo.New()
			e.HTTPErrorHandler = server.NewHTTPErrorHandler(slog.New(slog.DiscardHandler))
			e.Use(auth.JWTMiddleware(secret, func(echo.Context) bool { return false }))
			(&ToolApprovalHandler{turnService: verdictToolApprovalResponder{}}).Register(e)
			req := httptest.NewRequest(http.MethodPost, "/bots/"+approvalEntryBotID+"/tool-approvals/"+approvalEntryDecisionID+"/approve", strings.NewReader(`{}`))
			req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, body = %s; want %d", rec.Code, rec.Body.String(), tc.status)
			}
			if tc.wantCode != "" && !strings.Contains(rec.Body.String(), tc.wantCode) {
				t.Fatalf("body = %s, want code %s", rec.Body.String(), tc.wantCode)
			}
		})
	}
}

// A live view is one snapshot fanned out to every subscriber; each
// connection answers viewer_can_decide on its own copy and leaves the shared
// one untouched.
func TestRuntimeSubscriptionAnswersViewerCanDecidePerConnection(t *testing.T) {
	t.Parallel()

	approvals := func() []chatview.UIMessage {
		return []chatview.UIMessage{
			{ID: 1, Type: chatview.UIMessageText, Content: "working"},
			{ID: 2, Type: chatview.UIMessageTool, Approval: &chatview.UIToolApproval{ApprovalID: "live", Status: "pending", PendingAlive: true}},
			{ID: 3, Type: chatview.UIMessageTool, Approval: &chatview.UIToolApproval{ApprovalID: "decided", Status: "approved"}},
		}
	}
	shared := &sessionruntime.Snapshot{CurrentRunView: &sessionruntime.CurrentRunView{RunID: approvalEntryRunID, Messages: approvals()}}
	delta := &sessionruntime.RuntimeDelta{MessageUpserts: approvals()}
	var asked []string
	decider := func(viewer bool) runtimeViewerDecider {
		return func(_ context.Context, approvalID string) bool {
			asked = append(asked, approvalID)
			return viewer
		}
	}
	for _, tc := range []struct {
		name   string
		decide runtimeViewerDecider
		want   bool
	}{
		{"viewer who may decide", decider(true), true},
		{"viewer who may not", decider(false), false},
		{"no decider", nil, false},
	} {
		asked = nil
		frame := withViewerDecisions(context.Background(), runtimeOutboundEvent{Snapshot: shared, Delta: delta}, tc.decide)
		for _, messages := range [][]chatview.UIMessage{frame.Snapshot.CurrentRunView.Messages, frame.Delta.MessageUpserts} {
			if got := messages[1].Approval.ViewerCanDecide; got != tc.want {
				t.Fatalf("%s: live viewer_can_decide = %t, want %t", tc.name, got, tc.want)
			}
			if messages[2].Approval.ViewerCanDecide {
				t.Fatalf("%s: decided approval offered to the viewer", tc.name)
			}
		}
		if tc.decide != nil && (len(asked) != 2 || asked[0] != "live" || asked[1] != "live") {
			t.Fatalf("%s: decider asked about %v, want only the live approval", tc.name, asked)
		}
	}
	if shared.CurrentRunView.Messages[1].Approval.ViewerCanDecide || delta.MessageUpserts[1].Approval.ViewerCanDecide {
		t.Fatal("a connection wrote its answer into the shared view")
	}
	// A frame without a live approval goes out as it came.
	plain := runtimeOutboundEvent{Delta: &sessionruntime.RuntimeDelta{MessageUpserts: approvals()[:1]}}
	if got := withViewerDecisions(context.Background(), plain, decider(true)); got.Delta != plain.Delta {
		t.Fatal("a frame without a live approval was copied")
	}
}

type recordingApprovalDecider struct {
	principal agentapi.ToolApprovalPrincipal
	approval  toolapproval.Request
	allow     bool
	err       error
}

func (d *recordingApprovalDecider) CanDecideToolApproval(_ context.Context, approval toolapproval.Request, principal agentapi.ToolApprovalPrincipal) (bool, error) {
	d.approval, d.principal = approval, principal
	return d.allow, d.err
}

// History asks the decision authorization itself about the account reading
// the page, and renders a failed check read-only.
func TestHistoryViewerCanDecideAsksTheDecisionAuthority(t *testing.T) {
	t.Parallel()

	decider := &recordingApprovalDecider{allow: true}
	h := &MessageHandler{toolApprovalDecider: decider, logger: slog.New(slog.DiscardHandler)}
	approval := toolapproval.Request{ID: "approval-1", BotID: approvalEntryBotID, RunID: approvalEntryRunID}
	if !h.viewerCanDecideFn(context.Background(), approvalEntryInitiator)(approval) {
		t.Fatal("allowed viewer rendered read-only")
	}
	if decider.principal.UserID != approvalEntryInitiator || decider.approval.RunID != approvalEntryRunID {
		t.Fatalf("authority asked about %+v for %+v", decider.approval, decider.principal)
	}
	decider.err = errors.New("grant lookup failed")
	if h.viewerCanDecideFn(context.Background(), approvalEntryInitiator)(approval) {
		t.Fatal("a failed check offered the decision")
	}
	if (&MessageHandler{}).viewerCanDecideFn(context.Background(), approvalEntryInitiator) != nil {
		t.Fatal("a handler without the authority must leave viewer_can_decide unset")
	}
}
