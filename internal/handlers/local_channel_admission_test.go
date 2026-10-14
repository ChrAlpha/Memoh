package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/application"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	"github.com/felinics/memoh/internal/agent/turn"
	chatview "github.com/felinics/memoh/internal/agent/view"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
)

const (
	wsAdmissionBotID     = "11111111-1111-4111-8111-111111111111"
	wsAdmissionSessionID = "22222222-2222-4222-8222-222222222222"
)

type wsLifecycleQueries struct {
	dbstore.Queries
	params  []sqlc.CreateContextLifecycleParams
	upserts []sqlc.UpsertTerminalContextLifecycleParams
}

// GetDiscussSession reports no discuss session: every run end asks, and these
// runs are chat runs.
func (*wsLifecycleQueries) GetDiscussSession(context.Context, sqlc.GetDiscussSessionParams) (sqlc.GetDiscussSessionRow, error) {
	return sqlc.GetDiscussSessionRow{}, pgx.ErrNoRows
}

func (q *wsLifecycleQueries) CreateContextLifecycle(
	_ context.Context,
	arg sqlc.CreateContextLifecycleParams,
) (sqlc.CreateContextLifecycleRow, error) {
	q.params = append(q.params, arg)
	return sqlc.CreateContextLifecycleRow{
		RunID:     arg.RunID,
		BotID:     arg.BotID,
		SessionID: arg.SessionID,
		Status:    arg.Status,
		ErrorCode: arg.ErrorCode,
	}, nil
}

func (*wsLifecycleQueries) GetContextLifecycleByRunID(
	context.Context,
	pgtype.UUID,
) (sqlc.GetContextLifecycleByRunIDRow, error) {
	return sqlc.GetContextLifecycleByRunIDRow{}, pgx.ErrNoRows
}

// A run's terminal write asks for the decisions it left open; these runs left
// none.
func (*wsLifecycleQueries) ListToolApprovalsByRun(context.Context, pgtype.UUID) ([]sqlc.ToolApprovalRequest, error) {
	return nil, nil
}

func (*wsLifecycleQueries) ListUserInputsByRun(context.Context, pgtype.UUID) ([]sqlc.UserInputRequest, error) {
	return nil, nil
}

func (*wsLifecycleQueries) GetLatestAssistantContextLifecycleMetadataByRunID(context.Context, pgtype.UUID) ([]byte, error) {
	return nil, pgx.ErrNoRows
}

// The session runtime's terminal observer reconciles the row the run left.
func (q *wsLifecycleQueries) UpsertTerminalContextLifecycle(
	_ context.Context,
	arg sqlc.UpsertTerminalContextLifecycleParams,
) (sqlc.UpsertTerminalContextLifecycleRow, error) {
	q.upserts = append(q.upserts, arg)
	return sqlc.UpsertTerminalContextLifecycleRow{RunID: arg.RunID, Status: arg.Status, ErrorCode: arg.ErrorCode}, nil
}

type wsAdmissionFence struct{}

func (wsAdmissionFence) Activate(context.Context, string, string, int64) error { return nil }

// wsRunHarness is the handler wired the way production wires it: the
// application admits and drives every Web turn, against a real session
// runtime over an in-memory ledger. Every run it starts fails in its setup,
// which is what the tests below rely on to end a run they admitted.
type wsRunHarness struct {
	handler *LocalChannelHandler
	manager *sessionruntime.Manager
	runs    *sessionledger.Store
	writer  *wsWriter
}

func newWSRunHarness(t *testing.T, queries dbstore.Queries) wsRunHarness {
	t.Helper()
	runs := sessionledger.New()
	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID:       "owner-ws-admission",
		OwnerLeaseTTL: time.Minute,
		Ledger:        runs,
		Fence:         wsAdmissionFence{},
	})
	t.Cleanup(func() { _ = manager.Close() })
	agentService := application.NewService(slog.New(slog.DiscardHandler), nil, queries, nil, nil, nil, nil, time.UTC, time.Second)
	agentService.SetSessionRuntime(manager)
	// The run's setup reads the session first; failing that read ends every
	// run these tests start in its setup, before anything executes.
	agentService.SetSessionService(failingSessionService{err: errors.New("resolve: model not found")})
	return wsRunHarness{
		handler: &LocalChannelHandler{logger: slog.New(slog.DiscardHandler), agentService: agentService, sessionRuntime: manager},
		manager: manager,
		runs:    runs,
		writer:  &wsWriter{ch: make(chan []byte, 32), stop: make(chan struct{}), done: make(chan struct{})},
	}
}

func wsAdmissionTestRef() wsTurnRef {
	return wsTurn("invocation-1", wsAdmissionSessionID)
}

func wsAdmissionTestSubmission() []byte {
	return wsSubmission{Kind: "message", SessionID: wsAdmissionSessionID, Text: "hi"}.encode()
}

func emptyWSAdmissionView(context.Context, sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error) {
	return sessionruntime.RunAdmissionView{}, nil
}

// admit runs one admission the way the message case does.
func (h wsRunHarness) admit(ref wsTurnRef, submission []byte, build func(context.Context, sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error)) (*application.WebAdmission, wsTurnRef) {
	return h.handler.admitWSRun(context.Background(), h.writer, wsAdmissionBotID, ref, "ws.message", submission, build, nil)
}

// send runs an admitted Web send to its end and returns once the session has
// released it.
func (h wsRunHarness) send(t *testing.T, admitted *application.WebAdmission, ref wsTurnRef) {
	t.Helper()
	ended := make(chan struct{})
	h.handler.agentService.RunWebSend(admitted, application.ChatRequest{
		BotID: wsAdmissionBotID, ChatID: wsAdmissionBotID, ThreadID: wsAdmissionSessionID, Query: "hi",
	}, newWSRunDelivery(h.writer, ref, func() { close(ended) }))
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the Web run did not end")
	}
}

// frames drains what the socket was sent.
func (h wsRunHarness) frames(t *testing.T) []map[string]any {
	t.Helper()
	var frames []map[string]any
	for {
		select {
		case data := <-h.writer.ch:
			var frame map[string]any
			if err := json.Unmarshal(data, &frame); err != nil {
				t.Fatalf("decode frame: %v", err)
			}
			frames = append(frames, frame)
		default:
			return frames
		}
	}
}

func (h wsRunHarness) snapshotRun(t *testing.T) *sessionruntime.CurrentRunView {
	t.Helper()
	snapshot, err := h.manager.Snapshot(context.Background(), wsAdmissionBotID, wsAdmissionSessionID)
	if err != nil || snapshot.CurrentRunView == nil {
		t.Fatalf("snapshot = %+v, %v", snapshot, err)
	}
	return snapshot.CurrentRunView
}

// The end-to-end shape of one accepted turn: admission names the run and its
// turn, the client is told both plus the cursor the run became observable at
// before anything executes, a failure is reported by code on the initiating
// socket, and the run's end releases the session so the next turn is admitted.
func TestWSRunAnnouncesTheAdmittedTurnAndReleasesTheSession(t *testing.T) {
	t.Parallel()

	h := newWSRunHarness(t, nil)
	admitted, ref := h.admit(wsAdmissionTestRef(), wsAdmissionTestSubmission(), emptyWSAdmissionView)
	if admitted == nil || ref.RunID == "" || ref.RunID != admitted.RunID {
		t.Fatalf("admission = %+v, ref = %+v, want the admitted run", admitted, ref)
	}
	frames := h.frames(t)
	if len(frames) != 1 {
		t.Fatalf("frames before execution = %#v, want only the acceptance", frames)
	}
	accepted := frames[0]
	if accepted["type"] != "run_accepted" || accepted["run_id"] != admitted.RunID || accepted["turn_id"] != admitted.TurnID {
		t.Fatalf("run_accepted = %#v, want the admitted run and turn", accepted)
	}
	if accepted["epoch"] != admitted.Cursor.Epoch || accepted["epoch"] == "" {
		t.Fatalf("run_accepted cursor = %#v, want the activation cursor %+v", accepted, admitted.Cursor)
	}
	if _, present := accepted["duplicate"]; present {
		t.Fatalf("a first acceptance marked itself duplicate: %#v", accepted)
	}

	h.send(t, admitted, ref)

	frames = h.frames(t)
	if len(frames) != 1 || frames[0]["type"] != "error" || frames[0]["run_id"] != admitted.RunID {
		t.Fatalf("frames = %#v, want the run's failure on the initiating socket", frames)
	}
	if got := frames[0]["code"]; got != string(apperror.CodeRuntimeRunFailed) {
		t.Fatalf("failure frame code = %#v, want %s", got, apperror.CodeRuntimeRunFailed)
	}
	run, err := h.runs.Get(context.Background(), admitted.RunID)
	if err != nil || run.State != ledger.StateFailed || run.ErrorCode != string(apperror.CodeRuntimeRunFailed) {
		t.Fatalf("session_runs = %+v, %v; want failed %s", run, err, apperror.CodeRuntimeRunFailed)
	}

	next, _ := h.admit(wsTurn("invocation-2", wsAdmissionSessionID), wsSubmission{Kind: "message", SessionID: wsAdmissionSessionID, Text: "again"}.encode(), emptyWSAdmissionView)
	if next == nil || next.RunID == admitted.RunID {
		t.Fatalf("next admission = %+v, want a new run once the first released the session", next)
	}
	h.send(t, next, wsTurn("invocation-2", wsAdmissionSessionID).withRun(next.RunID))
}

func TestAdmitWSRunBuildsNormalMessageRequestUserTurn(t *testing.T) {
	t.Parallel()

	h := newWSRunHarness(t, nil)
	timestamp := time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC)
	messageAdmission := &wsMessageAdmission{
		handler: h.handler,
		botID:   wsAdmissionBotID,
		request: chatview.UITurn{
			Role:              "user",
			Text:              "hello",
			UserMessageKind:   "text",
			Timestamp:         timestamp,
			Platform:          "local",
			SenderUserID:      "user-1",
			ExternalMessageID: "invocation-1",
		},
		attachments: []turn.Attachment{{
			Type:   "file",
			Name:   "notes.txt",
			Mime:   "text/plain",
			Base64: "not-runtime-state",
		}},
	}

	admitted, ref := h.admit(wsAdmissionTestRef(), wsAdmissionTestSubmission(), messageAdmission.build)
	if admitted == nil {
		t.Fatal("admission failed")
	}
	defer h.send(t, admitted, ref)

	view := h.snapshotRun(t)
	if len(view.UserTurns) != 1 {
		t.Fatalf("request user turns = %#v, want one", view.UserTurns)
	}
	requestTurn := view.UserTurns[0]
	if requestTurn.TurnID != admitted.TurnID || requestTurn.Role != "user" || requestTurn.Text != "hello" || requestTurn.UserMessageKind != "text" ||
		!requestTurn.Timestamp.Equal(timestamp) || requestTurn.Platform != "local" ||
		requestTurn.SenderUserID != "user-1" || requestTurn.ExternalMessageID != "invocation-1" {
		t.Fatalf("request user turn = %#v", requestTurn)
	}
	if len(requestTurn.Attachments) != 1 || requestTurn.Attachments[0].Name != "notes.txt" ||
		requestTurn.Attachments[0].Mime != "text/plain" {
		t.Fatalf("request user turn attachments = %#v", requestTurn.Attachments)
	}
	prepared := messageAdmission.preparedAttachments()
	if len(prepared) != 1 || prepared[0].Base64 != "not-runtime-state" {
		t.Fatalf("prepared execution attachments = %#v", prepared)
	}
}

func TestAdmitWSRunPublishesReplacementOperation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		admission       *wsReplacementAdmission
		wantKind        string
		wantReplaceFrom string
		wantReplacement bool
	}{
		{
			name: "retry",
			admission: &wsReplacementAdmission{
				kind: sessionruntime.RunOperationRetry,
				prepareAnchor: func(context.Context) (string, error) {
					return "assistant-first", nil
				},
			},
			wantKind:        sessionruntime.RunOperationRetry,
			wantReplaceFrom: "assistant-first",
		},
		{
			name: "edit",
			admission: &wsReplacementAdmission{
				kind: sessionruntime.RunOperationEdit,
				prepareAnchor: func(context.Context) (string, error) {
					return "user-request", nil
				},
				replacementUserTurn: &chatview.UITurn{Role: "user", Text: "edited"},
				attachmentsPrepared: true,
			},
			wantKind:        sessionruntime.RunOperationEdit,
			wantReplaceFrom: "user-request",
			wantReplacement: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			h := newWSRunHarness(t, nil)
			admitted, ref := h.admit(wsAdmissionTestRef(), wsAdmissionTestSubmission(), test.admission.build)
			if admitted == nil {
				t.Fatal("admission failed")
			}
			defer h.send(t, admitted, ref)

			operation := h.snapshotRun(t).Operation
			if operation == nil {
				t.Fatal("replacement operation is nil")
			}
			if operation.Kind != test.wantKind || operation.ReplaceFromMessageID != test.wantReplaceFrom {
				t.Fatalf("replacement operation = %#v", operation)
			}
			if test.wantReplacement {
				if operation.ReplacementUserTurn == nil ||
					operation.ReplacementUserTurn.TurnID != admitted.TurnID ||
					operation.ReplacementUserTurn.Text != "edited" {
					t.Fatalf("replacement user turn = %#v", operation.ReplacementUserTurn)
				}
			} else if operation.ReplacementUserTurn != nil {
				t.Fatalf("replacement user turn = %#v, want nil", operation.ReplacementUserTurn)
			}
		})
	}
}

// The wire form is the contract. turn_id, epoch and seq use the names the
// subscription frames use, so a client that also observes the session orders
// acceptance against those frames with one comparison instead of two
// vocabularies (SR-OBS-002, SR-OBS-003).
func TestSendWSRunAcceptedCarriesTurnAndCursor(t *testing.T) {
	t.Parallel()

	accepted := decodeWSTestEvent(t, func(writer *wsWriter) {
		sendWSRunAccepted(writer, wsAdmissionTestRef().withRun("run-1"), wsRunAcceptance{
			TurnID: "turn-1",
			Cursor: sessionruntime.Cursor{Epoch: "epoch-1", Seq: 12},
		})
	})
	if accepted["type"] != "run_accepted" || accepted["run_id"] != "run-1" || accepted["invocation_id"] != "invocation-1" {
		t.Fatalf("run_accepted = %#v", accepted)
	}
	if accepted["turn_id"] != "turn-1" {
		t.Fatalf("run_accepted turn_id = %#v, want turn-1", accepted["turn_id"])
	}
	if accepted["epoch"] != "epoch-1" || accepted["seq"] != float64(12) {
		t.Fatalf("run_accepted cursor = %#v, want epoch-1 at seq 12", accepted)
	}
	if _, present := accepted["duplicate"]; present {
		t.Fatalf("a first acceptance marked itself duplicate: %#v", accepted)
	}
}

// A replay names the turn the original submission was admitted under, because
// every subscriber of that run is shown the same turn. It reports no cursor: the
// call reserved nothing, so it produced no position, and where the session
// stands now is what a subscription's snapshot answers.
func TestAdmitWSRunReplayNamesTheTurnWithoutACursor(t *testing.T) {
	t.Parallel()

	h := newWSRunHarness(t, nil)
	first, ref := h.admit(wsAdmissionTestRef(), wsAdmissionTestSubmission(), emptyWSAdmissionView)
	if first == nil {
		t.Fatal("admission failed")
	}
	h.send(t, first, ref)
	h.frames(t)

	again, _ := h.admit(wsAdmissionTestRef(), wsAdmissionTestSubmission(), emptyWSAdmissionView)
	if again != nil {
		t.Fatalf("replay = %+v, want an answered replay that executes nothing", again)
	}
	frames := h.frames(t)
	if len(frames) != 1 {
		t.Fatalf("frames = %#v, want one acceptance", frames)
	}
	accepted := frames[0]
	if accepted["type"] != "run_accepted" || accepted["run_id"] != first.RunID || accepted["duplicate"] != true {
		t.Fatalf("replay acceptance = %#v", accepted)
	}
	if accepted["turn_id"] != first.TurnID {
		t.Fatalf("replay acceptance turn_id = %#v, want %s", accepted["turn_id"], first.TurnID)
	}
	if _, present := accepted["epoch"]; present {
		t.Fatalf("replay reported a cursor it did not produce: %#v", accepted)
	}
	if _, present := accepted["seq"]; present {
		t.Fatalf("replay reported a cursor it did not produce: %#v", accepted)
	}
}

// What the client submitted is what the ledger keys the run on: the
// connection's session and the client's invocation.
func TestAdmitWSRunSubmitsTheClientsIdentity(t *testing.T) {
	t.Parallel()

	h := newWSRunHarness(t, nil)
	admitted, ref := h.admit(wsAdmissionTestRef(), wsAdmissionTestSubmission(), emptyWSAdmissionView)
	if admitted == nil {
		t.Fatal("admission failed")
	}
	defer h.send(t, admitted, ref)

	run, err := h.runs.GetByInvocation(context.Background(), wsAdmissionSessionID, "invocation-1")
	if err != nil || run.RunID != admitted.RunID {
		t.Fatalf("ledger run by invocation = %+v, %v; want run %s", run, err, admitted.RunID)
	}
}

// A refusal is not a run. The client is given the stable code it branches on —
// one of these is worth retrying unchanged, the other never is — and no run id,
// because none exists.
func TestAdmitWSRunRejectionsCarryStableCodes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		ref  wsTurnRef
		want apperror.Code
	}{
		{name: "busy", ref: wsTurn("invocation-2", wsAdmissionSessionID), want: apperror.CodeSessionBusy},
		{name: "conflict", ref: wsAdmissionTestRef(), want: apperror.CodeSessionInvocationConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newWSRunHarness(t, nil)
			running, runningRef := h.admit(wsAdmissionTestRef(), wsAdmissionTestSubmission(), emptyWSAdmissionView)
			if running == nil {
				t.Fatal("first admission failed")
			}
			defer h.send(t, running, runningRef)
			h.frames(t)

			different := wsSubmission{Kind: "message", SessionID: wsAdmissionSessionID, Text: "something else"}.encode()
			if refused, _ := h.admit(tc.ref, different, emptyWSAdmissionView); refused != nil {
				t.Fatalf("admission = %+v, want a refusal", refused)
			}
			frames := h.frames(t)
			if len(frames) != 1 {
				t.Fatalf("frames = %#v, want one rejection", frames)
			}
			rejected := frames[0]
			if rejected["type"] != "run_rejected" || rejected["code"] != string(tc.want) {
				t.Fatalf("rejection = %#v, want run_rejected %s", rejected, tc.want)
			}
			if _, present := rejected["run_id"]; present {
				t.Fatalf("rejection named a run: %#v", rejected)
			}
		})
	}
}

// Without durable admission there is no single-active-run guarantee to fall back
// on, so the turn is refused rather than run unprotected.
func TestAdmitWSRunRequiresConfiguredRuntime(t *testing.T) {
	t.Parallel()

	writer := &wsWriter{ch: make(chan []byte, 4), stop: make(chan struct{}), done: make(chan struct{})}
	handler := &LocalChannelHandler{
		logger:       slog.New(slog.DiscardHandler),
		agentService: application.NewService(slog.New(slog.DiscardHandler), nil, nil, nil, nil, nil, nil, time.UTC, time.Second),
	}
	released := false
	admitted, _ := handler.admitWSRun(context.Background(), writer, wsAdmissionBotID, wsAdmissionTestRef(), "ws.message", wsAdmissionTestSubmission(), emptyWSAdmissionView, func() { released = true })
	if admitted != nil {
		t.Fatalf("admission = %+v, want a refusal", admitted)
	}
	if !released {
		t.Fatal("a refused admission kept what the handler reserved for it")
	}
	var frame map[string]any
	if err := json.Unmarshal(<-writer.ch, &frame); err != nil || frame["type"] != "error" {
		t.Fatalf("frame = %#v, %v; want one error", frame, err)
	}
}

// A run that fails before its context was assembled still leaves a context
// lifecycle row once its terminal state is written, and the row keeps the
// failure's text private.
func TestWSRunPersistsPreContextFailureLifecycle(t *testing.T) {
	queries := &wsLifecycleQueries{}
	h := newWSRunHarness(t, queries)
	admitted, ref := h.admit(wsAdmissionTestRef(), wsAdmissionTestSubmission(), emptyWSAdmissionView)
	if admitted == nil {
		t.Fatal("admission failed")
	}
	h.send(t, admitted, ref)

	if run, err := h.runs.Get(context.Background(), admitted.RunID); err != nil || run.State != ledger.StateFailed {
		t.Fatalf("session_runs = %+v, %v; want failed", run, err)
	}
	if len(queries.params) != 0 || len(queries.upserts) != 1 {
		t.Fatalf("lifecycle writes = %d creates, %d terminal upserts; want the one terminal row", len(queries.params), len(queries.upserts))
	}
	row := queries.upserts[0]
	if row.Status != "failed_provider" {
		t.Fatalf("context lifecycle status = %q, want failed_provider", row.Status)
	}
	if bytes.Contains(row.Snapshot, []byte("resolve")) {
		t.Fatalf("content-light snapshot leaked private error: %s", row.Snapshot)
	}
	var snapshot struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(row.Snapshot, &snapshot); err != nil {
		t.Fatalf("decode lifecycle snapshot: %v", err)
	}
	if snapshot.Version != contextfrag.LifecycleSnapshotVersion {
		t.Fatalf("snapshot version = %d, want %d", snapshot.Version, contextfrag.LifecycleSnapshotVersion)
	}
}
