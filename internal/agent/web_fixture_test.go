package agent

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/felinics/memoh/internal/agent/agentapi"
	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/resume"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	chatview "github.com/felinics/memoh/internal/agent/view"
	"github.com/felinics/memoh/internal/agent/web"
	"github.com/felinics/memoh/internal/botbackup"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/dbtest"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/runtimefence"
	"github.com/felinics/memoh/internal/team"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
)

const (
	webTestBotID     = "11111111-1111-4111-8111-111111111111"
	webTestSessionID = "22222222-2222-4222-8222-222222222222"
)

// webTestDelivery is the socket's end of a Web run in these tests: it keeps
// every event the run delivered and says when the run has ended.
type webTestDelivery struct {
	mu     sync.Mutex
	events []agentapi.RunEvent
	ended  agentapi.RunEnded
	done   chan struct{}
}

func newWebTestDelivery() *webTestDelivery {
	return &webTestDelivery{done: make(chan struct{})}
}

func (d *webTestDelivery) Deliver(_ context.Context, ev agentapi.RunEvent) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, ev)
	return true
}

func (*webTestDelivery) Settle() {}

func (d *webTestDelivery) Ended(_ context.Context, end agentapi.RunEnded) {
	d.mu.Lock()
	d.ended = end
	d.mu.Unlock()
	close(d.done)
}

// wait returns once the run has ended, with what it delivered.
func (d *webTestDelivery) wait(t *testing.T) ([]agentapi.RunEvent, agentapi.RunEnded) {
	t.Helper()
	select {
	case <-d.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the Web run did not end")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]agentapi.RunEvent(nil), d.events...), d.ended
}

// payloads is what the run delivered as the stream of WSStreamEvent the
// pre-driver tests read.
func (d *webTestDelivery) payloads(t *testing.T) chan runreq.WSStreamEvent {
	t.Helper()
	events, _ := d.wait(t)
	ch := make(chan runreq.WSStreamEvent, len(events))
	for _, ev := range events {
		ch <- ev.Payload
	}
	close(ch)
	return ch
}

// streamWebSend runs req as an admitted Web send, the way the WebSocket does:
// admission names the run, and the send executes under it. It returns the
// events the socket's end received and the run's failure.
func streamWebSend(ctx context.Context, t *testing.T, svc *Service, req runreq.ChatRequest) (chan runreq.WSStreamEvent, error) {
	t.Helper()
	admission, err := admitWebRun(ctx, svc, agentapi.WebAdmitInput{
		BotID:        req.BotID,
		SessionID:    req.ThreadID,
		InvocationID: "web-invocation",
		Payload:      []byte(`{"kind":"message"}`),
	})
	if err != nil {
		t.Fatalf("AdmitWebRun() error = %v", err)
	}
	delivery := newWebTestDelivery()
	svc.web.RunSend(admission, req, delivery) //nolint:contextcheck // The run's context is the one admission derived from ctx.
	events := delivery.payloads(t)
	_, ended := delivery.wait(t)
	return events, ended.Failure
}

// webTestAdmission is the run a test's admitter starts for req: the run and
// turn the request names, or fixed ones when it names none.
func webTestAdmission(req runreq.ChatRequest) sessionruntime.Admission {
	admission := sessionruntime.Admission{RunID: req.RunID, TurnID: req.TurnID, Started: true}
	if admission.RunID == "" {
		admission.RunID = "run-1"
	}
	if admission.TurnID == "" {
		admission.TurnID = "turn-1"
	}
	if req.TurnPosition != nil {
		admission.TurnPosition = *req.TurnPosition
	}
	admission.Handle = sessionruntime.RunHandle{
		BotID: req.BotID, SessionID: req.ThreadID, RunID: admission.RunID, TurnID: admission.TurnID, FencingToken: 1,
	}
	return admission
}

// newWebTestRuntime is a service on a real session runtime over an in-memory
// ledger, so a Web run is admitted, published and finished the way production
// does it.
func newWebTestRuntime(t *testing.T, opts ...testOption) (*Service, *sessionruntime.Manager, *sessionledger.Store) {
	t.Helper()
	runs := sessionledger.New()
	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID:       "owner-web-test",
		OwnerLeaseTTL: time.Minute,
		Ledger:        runs,
		Fence:         agenttest.NoopFence{},
	})
	t.Cleanup(func() { _ = manager.Close() })
	svc := newTestService(t, Deps{SessionRuntime: manager},
		append([]testOption{withTurnAdmitter(manager), withManagerEvents(manager)}, opts...)...)
	return svc, manager, runs
}

func admitWebTestRun(t *testing.T, svc *Service, invocationID string) *web.Admission {
	t.Helper()
	admission, err := admitWebRun(context.Background(), svc, agentapi.WebAdmitInput{
		BotID:        webTestBotID,
		SessionID:    webTestSessionID,
		InvocationID: invocationID,
		Payload:      []byte(`{"kind":"message","text":"` + invocationID + `"}`),
	})
	if err != nil {
		t.Fatalf("AdmitWebRun(%s) error = %v", invocationID, err)
	}
	return admission
}

// webTestSpec is a Web send whose setup hands the driver exec.
func webTestSpec(admission *web.Admission, delivery agentapi.RunDelivery, exec drive.RunExecutor) drive.RunSpec {
	spec := web.RunSpec(admission, runreq.ChatRequest{BotID: webTestBotID, ThreadID: webTestSessionID}, delivery, outcome.InputHeld)
	spec.Setup = func(_ context.Context, req runreq.ChatRequest) (drive.RunPlan, error) {
		return drive.RunPlan{Exec: exec, Req: req}, nil
	}
	return spec
}

// blockingExecutor runs until its context ends, the way a model call that is
// still streaming does, and reports what ended it.
type blockingExecutor struct {
	started chan struct{}
	events  []native.StreamEvent
}

func (e *blockingExecutor) Execute(ctx context.Context, emit func(native.StreamEvent)) drive.ExecEnding {
	for _, ev := range e.events {
		emit(ev)
	}
	close(e.started)
	<-ctx.Done()
	terminal := native.StreamEvent{Type: native.EventAgentAbort, EndReason: event.EndCanceled}
	return drive.ExecEnding{
		Terminal: &terminal,
		Phase:    outcome.PhaseExecution,
		Commit: func(context.Context, *outcome.RunFailure, bool) (drive.CommitSummary, error) {
			return drive.CommitSummary{}, nil
		},
	}
}

func awaitStarted(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never started executing")
	}
}

// historyBackup is a backup of the bot's history the way an export writes it:
// the sessions and messages the exporter reads, as JSON entries of a zip.
func (h wsStepHistoryHarness) historyBackup(t *testing.T) []byte {
	t.Helper()
	ctx := context.Background()
	q := dbsqlc.New(h.pool)
	botID := dbpkg.ParseUUIDOrEmpty(h.botID)
	sessions, err := q.ListSessionsByBot(ctx, botID)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := q.ListAllMessagesForBackup(ctx, botID)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for path, value := range map[string]any{
		botbackup.ManifestPath:  botbackup.Manifest{SchemaVersion: botbackup.BackupSchemaVersion, SourceBotID: h.botID},
		"bot/profile.json":      map[string]string{"id": h.botID},
		"history/sessions.json": sessions,
		"history/messages.json": messages,
	} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		w, err := zw.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// liveSessions are the bot's sessions that are not deleted.
func (h wsStepHistoryHarness) liveSessions(t *testing.T) []string {
	t.Helper()
	rows, err := h.pool.Query(context.Background(), `SELECT id::text FROM bot_sessions WHERE bot_id = $1 AND deleted_at IS NULL`, h.botID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func (h wsStepHistoryHarness) startProcess(t *testing.T) resumeProcess {
	t.Helper()
	manager := h.restarted(t)
	service, _ := newWSStepHistoryService(t, wsStepHistorySuccess, postgresstore.NewQueriesWithPool(h.pool, dbsqlc.New(h.pool)), manager,
		wsStepHistoryFixtureOptions(
			withFixtureDeps(func(d *Deps) { d.RunCredentialSecret = "history-reset-resume" }),
			withFixtureOptions(withTeamScopes(runreq.SingleTeam(team.DefaultTeamID))),
			withFixtureQueries(func(q dbstore.Queries) dbstore.Queries {
				return resumeLedgerQueries{noDecisionQueries: q.(noDecisionQueries), db: dbsqlc.New(h.pool)}
			})))
	return resumeProcess{manager: manager, service: service}
}

// interruptedTurn admits a turn, saves its resume intent the way a streaming
// turn does, and stops it with the process's graceful shutdown.
func (h wsStepHistoryHarness) interruptedTurn(t *testing.T, p resumeProcess, task string) string {
	t.Helper()
	ctx := context.Background()
	admission, err := p.manager.Admit(ctx, sessionruntime.AdmitInput{
		BotID:        h.botID,
		SessionID:    h.sessionID,
		InvocationID: uuid.NewString(),
		Payload:      []byte(`{"kind":"message","text":"` + task + `"}`),
		Execution: sessionruntime.Execution{Admission: func(context.Context, sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error) {
			return sessionruntime.RunAdmissionView{}, nil
		}},
	})
	if err != nil || !admission.Started {
		t.Fatalf("admit turn = (%+v, %v)", admission, err)
	}
	// The request row a chat turn writes with its admission, in the run's turn.
	if _, err := h.pool.Exec(ctx, `INSERT INTO bot_history_messages (id, bot_id, session_id, role, content, run_id, turn_id, turn_position, turn_message_seq, turn_visible)
		VALUES ($1, $2, $3, 'user', '{}', $4, $5, $6, 1, true)`, uuid.NewString(), h.botID, h.sessionID, admission.RunID, admission.TurnID, admission.TurnPosition); err != nil {
		t.Fatalf("write request row: %v", err)
	}
	fenced := runtimefence.WithContext(ctx, runtimefence.Fence{BotID: h.botID, SessionID: h.sessionID, Token: admission.Handle.FencingToken})
	if err := p.service.admit.RecordResume(fenced, runreq.ChatRequest{RunID: admission.RunID, ChatID: h.botID, Query: task}); err != nil {
		t.Fatalf("record resume intent: %v", err)
	}
	if err := p.manager.InterruptForShutdown(ctx); err != nil {
		t.Fatalf("graceful shutdown: %v", err)
	}
	var state, code string
	if err := h.pool.QueryRow(ctx, `SELECT state, COALESCE(error_code, '') FROM session_runs WHERE run_id = $1`, admission.RunID).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "lost" || code != sessionruntime.RunErrorInterrupted {
		t.Fatalf("interrupted turn = (%q, %q), want lost with %s", state, code, sessionruntime.RunErrorInterrupted)
	}
	return admission.RunID
}

// resumeScan runs one pass of the process's startup resume worker and reports
// whether it admitted a continuation of runID.
func (h wsStepHistoryHarness) resumeScan(t *testing.T, p resumeProcess, runID string) bool {
	t.Helper()
	ctx := context.Background()
	scan := resume.Scan{}
	if err := p.service.resume.ScanInterrupted(ctx, &scan); err != nil {
		t.Fatalf("resume scan: %v", err)
	}
	// The scan only wakes pending; asking it here as well settles the session
	// before the ledger is read, whichever of the two gets there first.
	if _, err := p.service.pending.CheckAndAdmitDiscuss(ctx, h.botID, h.sessionID); err != nil {
		t.Fatalf("admit pending: %v", err)
	}
	var resumed string
	err := h.pool.QueryRow(ctx, `SELECT run_id::text FROM session_runs WHERE session_id = $1 AND invocation_id = $2`, h.sessionID, "resume:"+runID).Scan(&resumed)
	if err != nil {
		return false
	}
	// Let the continuation finish before the test tears its runtime down.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var state string
		if err := h.pool.QueryRow(ctx, `SELECT state FROM session_runs WHERE run_id = $1`, resumed).Scan(&state); err == nil && state != "accepted" && state != "running" && state != "finishing" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

// After a history clear the run a client sees must be gone: from the live
// projection, from a subscriber that was watching when the history went, and
// from a runtime that lost its live state and falls back to the ledger. The
// clear is the one DELETE /bots/{bot_id}/messages performs: the runtime reset
// gate, the deletion inside it, then the release.

type historyResetScope string

const (
	historyResetSession historyResetScope = "session"
	historyResetBot     historyResetScope = "bot"
)

// clearHistory runs the clear; during, when set, runs inside the reset before
// the history is deleted.
func (h wsStepHistoryHarness) clearHistory(t *testing.T, scope historyResetScope, during func()) {
	t.Helper()
	if err := h.clearHistoryErr(context.Background(), scope, during); err != nil {
		t.Fatal(err)
	}
}

func (h wsStepHistoryHarness) clearHistoryErr(ctx context.Context, scope historyResetScope, during func()) error {
	var (
		resetCtx context.Context
		release  func()
		err      error
	)
	if scope == historyResetBot {
		resetCtx, release, err = h.manager.BeginBotHistoryReset(ctx, h.botID)
	} else {
		resetCtx, release, err = h.manager.BeginSessionHistoryReset(ctx, h.botID, h.sessionID)
	}
	if err != nil {
		return fmt.Errorf("begin %s history reset: %w", scope, err)
	}
	defer release()
	if during != nil {
		during()
	}
	if scope == historyResetBot {
		err = h.messages.DeleteByBot(resetCtx, h.botID)
	} else {
		err = h.messages.DeleteBySession(resetCtx, h.sessionID)
	}
	if err != nil {
		return fmt.Errorf("delete %s history: %w", scope, err)
	}
	return nil
}

// restarted is a second runtime over the same PostgreSQL ledger whose live
// backend holds nothing: a process restart on the memory backend, or Redis
// state loss.
func (h wsStepHistoryHarness) restarted(t *testing.T) *sessionruntime.Manager {
	t.Helper()
	queries := postgresstore.NewQueriesWithPool(h.pool, dbsqlc.New(h.pool))
	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID:       "owner-restarted-" + uuid.NewString(),
		StateTTL:      time.Minute,
		OwnerLeaseTTL: time.Minute,
		Ledger:        ledger.NewPostgres(dbsqlc.New(h.pool), h.pool),
		Fence:         runtimefence.NewActivator(queries),
	})
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func mustSnapshot(t *testing.T, manager *sessionruntime.Manager, botID, sessionID string) sessionruntime.Snapshot {
	t.Helper()
	snapshot, err := manager.Snapshot(context.Background(), botID, sessionID)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	return snapshot
}

func describeRun(run *sessionruntime.CurrentRunView) string {
	if run == nil {
		return "<none>"
	}
	return run.RunID + " status=" + run.Status + " error_code=" + run.ErrorCode
}

// webRuntimeView follows a session the way the Web runtime client
// (apps/web/src/store/chat/runtime-client.ts) does, so a test sees the run the
// page shows rather than the frames it was sent: a snapshot applies only while
// the client awaits one or when it moves the cursor forward, and a dropped or
// closed stream makes the client subscribe again and await the next snapshot.
// The socket refuses to subscribe to a deleted session, and the client then
// keeps what it shows.
type webRuntimeView struct {
	t                *testing.T
	manager          *sessionruntime.Manager
	pool             *pgxpool.Pool
	botID, sessionID string
	sub              sessionruntime.Subscription
	awaiting         bool
	epoch            string
	seq              int64
	run              *sessionruntime.CurrentRunView
}

func (h wsStepHistoryHarness) watch(t *testing.T) *webRuntimeView {
	t.Helper()
	v := &webRuntimeView{t: t, manager: h.manager, pool: h.pool, botID: h.botID, sessionID: h.sessionID}
	v.resubscribe()
	t.Cleanup(func() { v.sub.Close() })
	return v
}

func (v *webRuntimeView) resubscribe() {
	v.t.Helper()
	if v.sub.Close != nil {
		v.sub.Close()
	}
	v.awaiting = true
	var deleted bool
	if err := v.pool.QueryRow(context.Background(), `SELECT deleted_at IS NOT NULL FROM bot_sessions WHERE id = $1`, v.sessionID).Scan(&deleted); err != nil {
		v.t.Fatal(err)
	}
	if deleted {
		v.sub = sessionruntime.Subscription{Close: func() {}}
		return
	}
	sub, err := v.manager.Subscribe(context.Background(), v.botID, v.sessionID)
	if err != nil {
		v.t.Fatalf("Subscribe() error = %v", err)
	}
	v.sub = sub
}

func (v *webRuntimeView) apply(event sessionruntime.Event, open bool) {
	v.t.Helper()
	switch {
	case !open || event.Type == sessionruntime.EventRuntimeDropped:
		if !v.awaiting || !open {
			v.resubscribe()
		}
	case event.Type == sessionruntime.EventRuntimeSnapshot && event.Snapshot != nil:
		if !v.awaiting && event.Epoch == v.epoch && event.Seq <= v.seq {
			return
		}
		v.awaiting = false
		v.epoch, v.seq, v.run = event.Epoch, event.Seq, event.Snapshot.CurrentRunView
	case event.Type == sessionruntime.EventRuntimeDelta && event.Delta != nil && !v.awaiting:
		switch {
		case event.Epoch != v.epoch || event.Seq > v.seq+1:
			v.resubscribe()
			return
		case event.Seq <= v.seq:
			return
		}
		v.seq = event.Seq
		if event.Delta.CurrentRunView != nil {
			v.run = event.Delta.CurrentRunView
		}
		if patch := event.Delta.Run; patch != nil && v.run != nil && v.run.RunID == patch.RunID {
			run := *v.run
			if patch.Status != nil {
				run.Status = *patch.Status
			}
			if patch.ErrorCode != nil {
				run.ErrorCode = *patch.ErrorCode
			}
			v.run = &run
		}
	}
}

// follow applies what the stream delivers for d, or until settled returns
// true, and reports whether it did.
func (v *webRuntimeView) follow(d time.Duration, settled func() bool) bool {
	v.t.Helper()
	deadline := time.After(d)
	for settled == nil || !settled() {
		select {
		case event, open := <-v.sub.C:
			v.apply(event, open)
		case <-deadline:
			return settled == nil
		}
	}
	return true
}

// drain applies everything the client has been sent so far.
func (v *webRuntimeView) drain() { v.follow(200*time.Millisecond, nil) }

// awaitReload lets a reload the reset triggered reach the client before the
// test goes on; a reset that does not trigger one leaves nothing to wait for.
func (v *webRuntimeView) awaitReload() {
	epoch, seq := v.epoch, v.seq
	v.follow(time.Second, func() bool { return !v.awaiting && (v.epoch != epoch || v.seq != seq) })
}

// awaitRun follows the stream until the client shows runID.
func (v *webRuntimeView) awaitRun(runID string) {
	v.t.Helper()
	if !v.follow(5*time.Second, func() bool { return !v.awaiting && v.run != nil && v.run.RunID == runID }) {
		v.t.Fatalf("client shows %s, want run %s", describeRun(v.run), runID)
	}
}

// awaitCleared follows the stream until the client shows no run, then checks
// nothing that follows brings the cleared run back.
func (v *webRuntimeView) awaitCleared(staleRunID string) {
	v.t.Helper()
	if !v.follow(5*time.Second, func() bool { return !v.awaiting && v.run == nil }) {
		v.t.Fatalf("client still shows %s after the clear", describeRun(v.run))
	}
	v.follow(300*time.Millisecond, nil)
	if v.run != nil && v.run.RunID == staleRunID {
		v.t.Fatalf("cleared run came back to the client: %s", describeRun(v.run))
	}
}

// failingUpdateBackend is the memory backend with writes, reads and a clock
// that can be made to fail the way a Redis outage fails them.
type failingUpdateBackend struct {
	*sessionruntime.MemoryBackend
	fail, failClock, failLoad atomic.Bool
}

func (b *failingUpdateBackend) Load(ctx context.Context, key sessionruntime.Key) (sessionruntime.Snapshot, bool, error) {
	if b.failLoad.Load() {
		return sessionruntime.Snapshot{}, false, errors.New("runtime backend unreachable")
	}
	return b.MemoryBackend.Load(ctx, key)
}

func (b *failingUpdateBackend) Update(ctx context.Context, key sessionruntime.Key, update sessionruntime.SnapshotUpdate) (sessionruntime.Snapshot, bool, error) {
	if b.fail.Load() {
		return sessionruntime.Snapshot{}, false, errors.New("runtime backend write failed")
	}
	return b.MemoryBackend.Update(ctx, key, update)
}

func (b *failingUpdateBackend) Now(ctx context.Context) (time.Time, error) {
	if b.failClock.Load() {
		return time.Time{}, errors.New("runtime backend unreachable")
	}
	return b.MemoryBackend.Now(ctx)
}

// These tests drive a Web chat turn the way the WebSocket handler does: the
// run is admitted by the real session runtime against the PostgreSQL ledger,
// the stream runs fenced so every completed step is committed to history as it
// lands, events are published to the manager as the forwarder publishes them,
// and the terminal write uses the outcome the runner returned. Each case pins
// what a client reads back after the run: the session_runs row, the history
// rows, and the turns the history endpoint builds from them.

const wsStepHistoryPartialText = "PARTIAL_WS_STEP_TEXT"

// wsStepHistoryLoopText is the chunk wsStepHistoryTextLoop repeats; it is long
// enough for one chunk to count toward the text loop guard's streak.
var wsStepHistoryLoopText = strings.Repeat("abcd", 64)

type wsStepHistoryModel string

const (
	wsStepHistorySuccess wsStepHistoryModel = "success"
	// wsStepHistoryAuthFailure rejects the request before any output.
	wsStepHistoryAuthFailure wsStepHistoryModel = "auth_failure"
	// wsStepHistoryBrokenStream streams some text, then the connection fails
	// with an error the runtime does not retry.
	wsStepHistoryBrokenStream wsStepHistoryModel = "broken_stream"
	// wsStepHistoryBlock streams some text, then waits until the request is
	// cancelled.
	wsStepHistoryBlock wsStepHistoryModel = "block"
	// wsStepHistoryToolThenBrokenStream answers the first call with a tool
	// call, which commits a step, and breaks the second call's stream after
	// some text.
	wsStepHistoryToolThenBrokenStream wsStepHistoryModel = "tool_then_broken_stream"
	// wsStepHistoryToolThenAuthFailure answers the first call with a tool
	// call, which commits a step, and rejects the second call before any
	// output.
	wsStepHistoryToolThenAuthFailure wsStepHistoryModel = "tool_then_auth_failure"
	// wsStepHistoryToolThenText answers the first call with a tool call, which
	// commits a step, and every later call with text.
	wsStepHistoryToolThenText wsStepHistoryModel = "tool_then_text"
	// wsStepHistoryTextLoop repeats the same text until the runtime's loop
	// detection aborts the run. The abort follows no error event, and its
	// cause carries no catalogued code.
	wsStepHistoryTextLoop wsStepHistoryModel = "text_loop"
	// wsStepHistoryNumbered answers every call with its own text, ANSWER-<n>
	// for the n-th streamed call, so a replaced answer is told from its
	// replacement.
	wsStepHistoryNumbered wsStepHistoryModel = "numbered"
)

// wsStepHistoryTransport answers streaming model requests with the scripted
// model behavior and passes every other request to the test provider.
type wsStepHistoryTransport struct {
	base  http.RoundTripper
	mode  wsStepHistoryModel
	calls atomic.Int32
}

func (t *wsStepHistoryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return nil, err
	}
	var request struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &request)
	if !request.Stream {
		clone := r.Clone(r.Context())
		clone.Body = io.NopCloser(strings.NewReader(string(body)))
		clone.ContentLength = int64(len(body))
		return t.base.RoundTrip(clone)
	}
	call := t.calls.Add(1)
	textChunk := fmt.Sprintf("data: {\"id\":\"chatcmpl-ws-step\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":null}]}\n\n", wsStepHistoryPartialText)
	stream := func(tail io.Reader) *http.Response {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(io.MultiReader(strings.NewReader(textChunk), tail)),
			Request:    r,
		}
	}
	switch t.mode {
	case wsStepHistoryToolThenBrokenStream, wsStepHistoryToolThenAuthFailure, wsStepHistoryToolThenText:
		if call == 1 {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(
					"data: {\"id\":\"chatcmpl-ws-step\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_ws_step\",\"type\":\"function\",\"function\":{\"name\":\"missing_tool\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\n" +
						"data: {\"id\":\"chatcmpl-ws-step\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")),
				Request: r,
			}, nil
		}
		if t.mode == wsStepHistoryToolThenBrokenStream {
			return stream(failingReader{err: errors.New("upstream stream broke")}), nil
		}
		if t.mode == wsStepHistoryToolThenText {
			return stream(strings.NewReader("data: {\"id\":\"chatcmpl-ws-step\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")), nil
		}
		fallthrough
	case wsStepHistoryAuthFailure:
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Status:     "401 Unauthorized",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"invalid api key"}}`)),
			Request:    r,
		}, nil
	case wsStepHistoryNumbered:
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(fmt.Sprintf(
				"data: {\"id\":\"chatcmpl-ws-step\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ANSWER-%d\"},\"finish_reason\":null}]}\n\n"+
					"data: {\"id\":\"chatcmpl-ws-step\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", call))),
			Request: r,
		}, nil
	case wsStepHistoryBrokenStream:
		return stream(failingReader{err: errors.New("upstream stream broke")}), nil
	case wsStepHistoryBlock:
		return stream(blockingReader{ctx: r.Context()}), nil
	case wsStepHistoryTextLoop:
		loopChunk := fmt.Sprintf("data: {\"id\":\"chatcmpl-ws-step\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", wsStepHistoryLoopText)
		return stream(io.MultiReader(strings.NewReader(strings.Repeat(loopChunk, 4)), blockingReader{ctx: r.Context()})), nil
	default:
		return stream(strings.NewReader("data: {\"id\":\"chatcmpl-ws-step\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")), nil
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

type blockingReader struct{ ctx context.Context }

func (r blockingReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

// loopDetectionQueries turns on the bot's loop detection.
type loopDetectionQueries struct{ noDecisionQueries }

func (loopDetectionQueries) GetBotByID(context.Context, pgtype.UUID) (dbsqlc.GetBotByIDRow, error) {
	return dbsqlc.GetBotByIDRow{Metadata: []byte(`{"features":{"loop_detection":{"enabled":true}}}`)}, nil
}

// wsStepHistoryRun is what a client can read back after one run.
type wsStepHistoryRun struct {
	// events are the types of the events the client was sent.
	events []string
	// failureFrames are the error and agent_abort events the client was
	// sent, each as its type and the code it carried.
	failureFrames []string
	// text is the text the client was streamed.
	text string
	// sessionRun is session_runs state / error_code / error_message.
	sessionRun [3]string
	// history is one line per history row: role, assistant text, and the
	// failure metadata the row carries.
	history []string
	// turns is one line per turn the history endpoint returns: role and the
	// messages it renders.
	turns []string
	// failureMeta is, for each assistant row a failure marked, the keys the
	// failure left in its metadata, so two write paths can be compared field
	// by field.
	failureMeta []map[string]any
}

type wsStepHistoryHarness struct {
	pool      *pgxpool.Pool
	botID     string
	sessionID string
	service   *Service
	manager   *sessionruntime.Manager
	messages  *messagepkg.DBService
}

// wsStepHistorySetup is what newWSStepHistoryService builds its service from;
// an option changes it before the service is assembled.
type wsStepHistorySetup struct {
	manager *sessionruntime.Manager
	// fixture are the options of the direct lifecycle fixture underneath.
	fixture []directLifecycleOption
	// messages wraps the PostgreSQL message store the service reads and writes.
	messages func(*messagepkg.DBService) messagepkg.Service
}

type wsStepHistoryOption func(*wsStepHistorySetup)

// wsStepHistoryFixtureOptions adds options of the direct lifecycle fixture.
func wsStepHistoryFixtureOptions(options ...directLifecycleOption) wsStepHistoryOption {
	return func(s *wsStepHistorySetup) { s.fixture = append(s.fixture, options...) }
}

func newWSStepHistoryHarness(t *testing.T, mode wsStepHistoryModel, opts ...wsStepHistoryOption) wsStepHistoryHarness {
	t.Helper()
	return newWSStepHistoryHarnessOn(t, mode, sessionruntime.NewMemoryBackend(), opts...)
}

func newWSStepHistoryHarnessOn(t *testing.T, mode wsStepHistoryModel, backend sessionruntime.Backend, opts ...wsStepHistoryOption) wsStepHistoryHarness {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.OpenPostgres(t, ctx)
	botID, sessionID := agenttest.CreateSessionFixture(t, ctx, pool)

	// History rows reference the model that produced them; the model itself
	// is resolved by the fixture's fake queries.
	if _, err := pool.Exec(ctx, `
		INSERT INTO providers (id, name) VALUES ($1, 'ws-step-history-provider')
		ON CONFLICT DO NOTHING
	`, directLifecycleProviderID); err != nil {
		t.Fatalf("create provider fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO models (id, model_id, provider_id) VALUES ($1, 'direct-lifecycle-model', $2)
		ON CONFLICT DO NOTHING
	`, directLifecycleModelID, directLifecycleProviderID); err != nil {
		t.Fatalf("create model fixture: %v", err)
	}
	queries := postgresstore.NewQueriesWithPool(pool, dbsqlc.New(pool))
	manager := sessionruntime.NewManager(backend, sessionruntime.Options{
		OwnerID:       "owner-ws-step-history",
		StateTTL:      time.Minute,
		OwnerLeaseTTL: time.Minute,
		Ledger:        ledger.NewPostgres(dbsqlc.New(pool), pool),
		Fence:         runtimefence.NewActivator(queries),
	})
	t.Cleanup(func() { _ = manager.Close() })
	service, messages := newWSStepHistoryService(t, mode, queries, manager, opts...)
	return wsStepHistoryHarness{pool: pool, botID: botID, sessionID: sessionID, service: service, manager: manager, messages: messages}
}

// newWSStepHistoryService is the application service of one server process:
// its runtime is manager and its history lives in queries.
func newWSStepHistoryService(t *testing.T, mode wsStepHistoryModel, queries dbstore.Queries, manager *sessionruntime.Manager, opts ...wsStepHistoryOption) (*Service, *messagepkg.DBService) {
	t.Helper()
	setup := wsStepHistorySetup{manager: manager}
	for _, opt := range opts {
		opt(&setup)
	}
	messages := messagepkg.NewService(slog.New(slog.DiscardHandler), queries)
	var stored messagepkg.Service = messages
	if setup.messages != nil {
		stored = setup.messages(messages)
	}
	transport := &wsStepHistoryTransport{mode: mode}
	client := &http.Client{Transport: transport}
	options := []directLifecycleOption{
		withFixtureMessages(stored),
		withFixtureOptions(withRunconfig(func(d *runconfig.Deps) { d.HTTPClient = client })),
		// The real manager asks for the run's pending decisions when it finishes.
		withFixtureRuntime(manager),
	}
	if mode == wsStepHistoryTextLoop {
		options = append(options, withFixtureQueries(func(q dbstore.Queries) dbstore.Queries {
			return loopDetectionQueries{q.(noDecisionQueries)}
		}))
	}
	fixture := newDirectLifecycleFixture(t, directLifecycleModelSuccess, append(options, setup.fixture...)...)
	transport.base = fixture.httpClient.Transport
	return fixture.service, messages
}

// run admits one Web chat turn and runs it to its terminal write, the way
// the WebSocket does. stop, when set, is called once the first text reaches
// the client and is expected to stop the run.
func (h wsStepHistoryHarness) run(t *testing.T, stop func(ctx context.Context, runID string)) wsStepHistoryRun {
	t.Helper()
	return h.runRequest(t, stop, nil)
}

// runRequest is run with the chat request adjusted by mutate, which the test
// uses to give the run its own input channel.
func (h wsStepHistoryHarness) runRequest(t *testing.T, stop func(ctx context.Context, runID string), mutate func(*runreq.ChatRequest)) wsStepHistoryRun {
	t.Helper()
	baseCtx := context.Background()
	admission, err := admitWebRun(baseCtx, h.service, agentapi.WebAdmitInput{
		BotID:        h.botID,
		SessionID:    h.sessionID,
		InvocationID: uuid.NewString(),
		Payload:      []byte(`{"kind":"message","text":"` + directLifecyclePrompt + `"}`),
	})
	if err != nil {
		t.Fatalf("AdmitWebRun() error = %v", err)
	}
	if admission.Duplicate() {
		t.Fatal("AdmitWebRun() did not start the run")
	}
	delivery := &wsStepHistoryDelivery{done: make(chan struct{})}
	if stop != nil {
		delivery.onText = func() { go stop(baseCtx, admission.RunID()) }
	}
	req := runreq.ChatRequest{
		BotID:           h.botID,
		ChatID:          h.botID,
		ThreadID:        h.sessionID,
		Query:           directLifecyclePrompt,
		RawQuery:        directLifecyclePrompt,
		UserVisibleText: directLifecyclePrompt,
		CurrentChannel:  "local",
	}
	if mutate == nil {
		h.service.web.RunSend(admission, req, delivery)
	} else {
		// The request is adjusted after admission wired it, which is the only
		// way to give the run an input channel of the test's own.
		spec := web.RunSpec(admission, req, delivery, outcome.InputHeld)
		mutate(&spec.Req)
		spec.Setup = func(ctx context.Context, req runreq.ChatRequest) (drive.RunPlan, error) {
			return h.service.plan.ChatSetup(ctx, req, spec)
		}
		h.service.web.Start(admission, spec)
	}
	select {
	case <-delivery.done:
	case <-time.After(20 * time.Second):
		t.Fatal("WS turn did not finish")
	}
	out := h.read(t, admission.RunID())
	out.events = delivery.events
	out.failureFrames = delivery.failureFrames
	out.text = delivery.text.String()
	return out
}

// wsStepHistoryDelivery is what the initiating socket is handed.
type wsStepHistoryDelivery struct {
	events        []string
	failureFrames []string
	text          strings.Builder
	onText        func()
	stopOnce      sync.Once
	done          chan struct{}
}

func (d *wsStepHistoryDelivery) Deliver(_ context.Context, ev agentapi.RunEvent) bool {
	event := ev.Event
	d.events = append(d.events, string(event.Type))
	if event.Type == native.EventTextDelta {
		d.text.WriteString(event.Delta)
		if d.onText != nil {
			d.stopOnce.Do(d.onText)
		}
	}
	if event.Type == native.EventError || event.Type == native.EventAgentAbort {
		d.failureFrames = append(d.failureFrames, string(event.Type)+" "+event.Code)
	}
	return true
}

func (*wsStepHistoryDelivery) Settle() {}

func (d *wsStepHistoryDelivery) Ended(context.Context, agentapi.RunEnded) { close(d.done) }

func (h wsStepHistoryHarness) read(t *testing.T, runID string) wsStepHistoryRun {
	t.Helper()
	ctx := context.Background()
	var out wsStepHistoryRun
	if err := h.pool.QueryRow(ctx, `
		SELECT state, COALESCE(error_code, ''), COALESCE(error_message, '')
		FROM session_runs WHERE run_id = $1
	`, runID).Scan(&out.sessionRun[0], &out.sessionRun[1], &out.sessionRun[2]); err != nil {
		t.Fatalf("read session_runs: %v", err)
	}
	rows, err := h.messages.ListBySession(ctx, h.sessionID)
	if err != nil {
		t.Fatalf("list history: %v", err)
	}
	for _, row := range rows {
		// The user row stores the model-facing text; the turns below show
		// what the user sees.
		line := row.Role
		if row.Role != "user" {
			line += " " + strconv.Quote(historyRowText(row))
		}
		if row.Metadata[messagepkg.AgentStepInterruptedMetadataKey] == true {
			line += " interrupted"
		}
		if code, _ := row.Metadata[messagepkg.HistoryErrorCodeMetadataKey].(string); code != "" {
			line += " error_code=" + code
		}
		out.history = append(out.history, line)
		if row.Role == "assistant" {
			meta := map[string]any{}
			for _, key := range []string{
				messagepkg.AgentStepInterruptedMetadataKey,
				messagepkg.HistoryErrorCodeMetadataKey,
				messagepkg.HistoryErrorArgsMetadataKey,
				messagepkg.HistoryFailureOriginMetadataKey,
				"error",
			} {
				if value, ok := row.Metadata[key]; ok {
					meta[key] = value
				}
			}
			if len(meta) > 0 {
				out.failureMeta = append(out.failureMeta, meta)
			}
		}
	}
	uiRows, err := h.messages.ListLatestUIBySession(ctx, h.sessionID, 30)
	if err != nil {
		t.Fatalf("list UI history: %v", err)
	}
	// The history endpoint reads newest first and reverses before converting.
	slices.Reverse(uiRows)
	for _, uiTurn := range chatview.ConvertMessagesToUITurns(uiRows) {
		var line strings.Builder
		line.WriteString(uiTurn.Role)
		if uiTurn.Text != "" {
			line.WriteString(" " + strconv.Quote(uiTurn.Text))
		}
		for _, message := range uiTurn.Messages {
			line.WriteString(" [" + string(message.Type))
			if message.Code != "" {
				line.WriteString(" " + message.Code)
			}
			if message.Type == chatview.UIMessageText {
				line.WriteString(" " + strconv.Quote(message.Content))
			}
			line.WriteString("]")
		}
		out.turns = append(out.turns, line.String())
	}
	return out
}

func historyRowText(row messagepkg.Message) string {
	var content struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(row.Content, &content); err != nil {
		return string(row.Content)
	}
	var text string
	if err := json.Unmarshal(content.Content, &text); err == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content.Content, &parts); err == nil {
		var b strings.Builder
		for _, part := range parts {
			if part.Type == "text" {
				b.WriteString(part.Text)
			}
		}
		return b.String()
	}
	return string(content.Content)
}

// admitWebRun admits through the service and hands back the concrete
// admission, for tests that start the run's spec themselves.
func admitWebRun(ctx context.Context, s *Service, in agentapi.WebAdmitInput) (*web.Admission, error) {
	admission, err := s.AdmitWebRun(ctx, in)
	if err != nil {
		return nil, err
	}
	return admission.(*web.Admission), nil
}
