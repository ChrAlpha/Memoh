package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/drive"
	"github.com/felinics/memoh/internal/agent/internal/runconfig"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/contextview"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/settings"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
	"github.com/felinics/memoh/internal/workspace/bridge"
)

// hookBridgeProvider hands the application a session runtime's workspace
// client, which is what the hooks service reads through.
type hookBridgeProvider struct{ client *bridge.Client }

func (p hookBridgeProvider) MCPClient(context.Context, string) (*bridge.Client, error) {
	return p.client, nil
}

// These tests read what an entry's failure leaves in the conversation, against
// the real PostgreSQL ledger. The row policy is one decision made from one
// place, so it is worth checking per entry: a Web send hands the draft back,
// while an IM sender has already sent their message and must be told why it was
// not answered.
//
// Each case is skipped unless TEST_POSTGRES_DSN names a database.

// seedDirectLifecycleModel gives the real store the provider and model rows the
// direct lifecycle fixture's runs persist their messages against.
func seedDirectLifecycleModel(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO providers (id, name) VALUES ($1, 'drive-history-provider')
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
}

// waitRunResults waits for the terminal observer, which runs after the turn's
// handle closes, and then requires exactly want result records.
func waitRunResults(t *testing.T, logs *agenttest.CapturedLogs, want int) []slog.Record {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(logs.RunResults()) < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// Give a duplicate observation the same chance to arrive.
	time.Sleep(50 * time.Millisecond)
	records := logs.RunResults()
	if len(records) != want {
		attrs := make([]map[string]string, 0, len(records))
		for _, record := range records {
			attrs = append(attrs, agenttest.RecordAttrs(record))
		}
		encoded, _ := json.Marshal(attrs)
		t.Fatalf("result records = %s, want %d", encoded, want)
	}
	return records
}

const (
	directLifecycleModelID    = "44444444-4444-4444-8444-444444444444"
	directLifecycleProviderID = "55555555-5555-4555-8555-555555555555"
	directLifecyclePrompt     = "PRIVATE_DIRECT_LIFECYCLE_PROMPT"
	directLifecycleResponse   = "PRIVATE_DIRECT_LIFECYCLE_RESPONSE"
)

type directLifecycleQueries struct {
	agenttest.ModelQueries
	modelID pgtype.UUID
}

func (q *directLifecycleQueries) GetSettingsByBotID(
	_ context.Context,
	botID pgtype.UUID,
) (sqlc.GetSettingsByBotIDRow, error) {
	return sqlc.GetSettingsByBotIDRow{
		BotID:           botID,
		ReasoningEffort: "medium",
		ChatModelID:     q.modelID,
	}, nil
}

// GetLatestContextLifecycleBySession reports no earlier run plan, so a
// turn's pressure is its rows and own messages.
// SaveSessionRunResumeContext accepts the resume intent every fenced chat run
// records.
func (*directLifecycleQueries) SaveSessionRunResumeContext(context.Context, sqlc.SaveSessionRunResumeContextParams) (int64, error) {
	return 1, nil
}

// GetSessionInterruptedRun reports that no session is owed a continuation.
func (*directLifecycleQueries) GetSessionInterruptedRun(context.Context, pgtype.UUID) (sqlc.SessionRun, error) {
	return sqlc.SessionRun{}, pgx.ErrNoRows
}

func (*directLifecycleQueries) GetLatestContextLifecycleBySession(context.Context, pgtype.UUID) ([]byte, error) {
	return nil, pgx.ErrNoRows
}

func (*directLifecycleQueries) GetBotByID(
	context.Context,
	pgtype.UUID,
) (sqlc.GetBotByIDRow, error) {
	return sqlc.GetBotByIDRow{}, pgx.ErrNoRows
}

func (*directLifecycleQueries) ListCompactionArtifactLineageBySession(
	context.Context,
	pgtype.UUID,
) ([]sqlc.BotHistoryMessageCompact, error) {
	return nil, nil
}

type directLifecycleFixture struct {
	service *Service
	// queries is the model catalog and run store the service reads, before any
	// fixture option wrapped it.
	queries *directLifecycleQueries
	// httpClient is what the service's model requests go through.
	httpClient *http.Client
	lifecycles *synchronizedLifecycleStore
	messages   *agenttest.RecordingMessages
	runtime    *agenttest.LifecycleAdmitter
	started    chan struct{}
	// cancelled and release belong to directLifecycleModelHoldOnCancel: the
	// model call signals cancelled when its request context ends, then holds
	// the handler open until release, so a test can observe the run while its
	// runtime is still cleaning up.
	cancelled chan struct{}
	release   chan struct{}
	// releaseOnce makes releasing the held model call idempotent: a test
	// releases it when it is done, and the fixture's own cleanup releases it
	// again so an assertion that failed first cannot leave the handler blocked
	// and hang the suite behind it.
	releaseOnce *sync.Once
	// requests is the model endpoint's record of the request bodies it
	// received. Chat entries resolve through the whole application path, so
	// what the model was asked for is how a test reads the run's routing,
	// identity, and query.
	requests *modelRequestRecorder
}

// modelRequestRecorder keeps the request bodies a model endpoint received.
type modelRequestRecorder struct {
	mu     sync.Mutex
	bodies []string
}

func (r *modelRequestRecorder) add(body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies = append(r.bodies, body)
}

// bodies returns the request bodies the endpoint received.
func (r *modelRequestRecorder) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.bodies...)
}

type synchronizedLifecycleStore struct {
	mu    sync.Mutex
	store agenttest.RecordingLifecycleStore
}

func (s *synchronizedLifecycleStore) CreateContextLifecycle(
	ctx context.Context,
	arg sqlc.CreateContextLifecycleParams,
) (sqlc.CreateContextLifecycleRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store.Existing != nil {
		return sqlc.CreateContextLifecycleRow{}, &pgconn.PgError{Code: "23505"}
	}
	return s.store.CreateContextLifecycle(ctx, arg)
}

func (s *synchronizedLifecycleStore) GetContextLifecycleByRunID(
	ctx context.Context,
	runID pgtype.UUID,
) (sqlc.GetContextLifecycleByRunIDRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.GetContextLifecycleByRunID(ctx, runID)
}

func (s *synchronizedLifecycleStore) GetLatestAssistantContextLifecycleMetadataByRunID(
	ctx context.Context,
	runID pgtype.UUID,
) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.GetLatestAssistantContextLifecycleMetadataByRunID(ctx, runID)
}

func (s *synchronizedLifecycleStore) UpdateAbortedContextLifecycleSnapshot(
	ctx context.Context,
	arg sqlc.UpdateAbortedContextLifecycleSnapshotParams,
) (sqlc.UpdateAbortedContextLifecycleSnapshotRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, err := s.store.UpdateAbortedContextLifecycleSnapshot(ctx, arg)
	if err == nil && s.store.Existing != nil {
		s.store.Existing.Snapshot = append([]byte(nil), arg.Snapshot...)
		for i := range s.store.Creates {
			if s.store.Creates[i].RunID == arg.RunID {
				s.store.Creates[i].Snapshot = append([]byte(nil), arg.Snapshot...)
			}
		}
	}
	return row, err
}

func (s *synchronizedLifecycleStore) UpsertAbortedContextLifecycle(
	ctx context.Context,
	arg sqlc.UpsertAbortedContextLifecycleParams,
) (sqlc.UpsertAbortedContextLifecycleRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.UpsertAbortedContextLifecycle(ctx, arg)
}

func (s *synchronizedLifecycleStore) UpsertTerminalContextLifecycle(
	ctx context.Context,
	arg sqlc.UpsertTerminalContextLifecycleParams,
) (sqlc.UpsertTerminalContextLifecycleRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.UpsertTerminalContextLifecycle(ctx, arg)
}

func (s *synchronizedLifecycleStore) ListTerminalSessionRunsNeedingContextLifecycle(
	ctx context.Context,
	batchSize int32,
) ([]sqlc.ListTerminalSessionRunsNeedingContextLifecycleRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.ListTerminalSessionRunsNeedingContextLifecycle(ctx, batchSize)
}

// creates are the lifecycle rows the run wrote: the ones written directly, and
// the ones the session runtime's terminal observer wrote once the run's fenced
// terminal write was accepted.
func (s *synchronizedLifecycleStore) creates() []sqlc.CreateContextLifecycleParams {
	s.mu.Lock()
	defer s.mu.Unlock()
	creates := make([]sqlc.CreateContextLifecycleParams, len(s.store.Creates), len(s.store.Creates)+len(s.store.TerminalUpserts))
	copy(creates, s.store.Creates)
	for _, upsert := range s.store.TerminalUpserts {
		creates = append(creates, sqlc.CreateContextLifecycleParams{
			RunID: upsert.RunID, BotID: upsert.BotID, SessionID: upsert.SessionID,
			Status: upsert.Status, ErrorCode: upsert.ErrorCode, Snapshot: upsert.Snapshot,
		})
	}
	for i := range creates {
		creates[i].Snapshot = append([]byte(nil), creates[i].Snapshot...)
	}
	return creates
}

type directLifecycleModelMode string

const (
	directLifecycleModelSuccess directLifecycleModelMode = "success"
	directLifecycleModelFailure directLifecycleModelMode = "failure"
	directLifecycleModelBlock   directLifecycleModelMode = "block"
	// directLifecycleModelHoldOnCancel streams a reply and then keeps the
	// handler open past the caller's cancellation, the way a provider that has
	// to finish writing its own state does. A test uses it to hold a run in its
	// cleanup window.
	directLifecycleModelHoldOnCancel directLifecycleModelMode = "hold_on_cancel"
)

// directLifecycleSetup is what newDirectLifecycleFixture builds its service
// from; an option changes it before the service is assembled.
type directLifecycleSetup struct {
	deps    Deps
	opts    []testOption
	queries []func(dbstore.Queries) dbstore.Queries
}

type directLifecycleOption func(*directLifecycleSetup)

// withFixtureDeps changes the Deps the fixture's service is built from.
func withFixtureDeps(f func(*Deps)) directLifecycleOption {
	return func(s *directLifecycleSetup) { f(&s.deps) }
}

// withFixtureOptions adds newTestService options to the fixture's service.
func withFixtureOptions(opts ...testOption) directLifecycleOption {
	return func(s *directLifecycleSetup) { s.opts = append(s.opts, opts...) }
}

// withFixtureQueries wraps the queries the fixture's service reads through; the
// wrappers apply in the order given.
func withFixtureQueries(wrap func(dbstore.Queries) dbstore.Queries) directLifecycleOption {
	return func(s *directLifecycleSetup) { s.queries = append(s.queries, wrap) }
}

// withFixtureMessages replaces the message store the fixture's service reads
// and writes.
func withFixtureMessages(messages messagepkg.Service) directLifecycleOption {
	return withFixtureDeps(func(d *Deps) { d.Messages = messages })
}

// withFixtureRuntime makes manager the session runtime of the fixture's service:
// it admits the runs, takes their events and writes their ending, and asks for
// the pending decisions of a run when it finishes.
func withFixtureRuntime(manager *sessionruntime.Manager) directLifecycleOption {
	return func(s *directLifecycleSetup) {
		withFixtureOptions(withSessionManager(manager))(s)
		withNoDecisionQueries()(s)
	}
}

// withNoDecisionQueries makes every run of the fixture's service end with no
// decisions pending.
func withNoDecisionQueries() directLifecycleOption {
	return withFixtureQueries(func(q dbstore.Queries) dbstore.Queries {
		if already, ok := q.(noDecisionQueries); ok {
			return already
		}
		return noDecisionQueries{directLifecycleQueries: q.(*directLifecycleQueries)}
	})
}

func newDirectLifecycleFixture(t *testing.T, mode directLifecycleModelMode, options ...directLifecycleOption) directLifecycleFixture {
	t.Helper()

	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := &sync.Once{}
	requests := &modelRequestRecorder{}
	var startedOnce sync.Once
	var cancelledOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream bool `json:"stream"`
		}
		body, _ := io.ReadAll(r.Body)
		recordRequest := func() { requests.add(string(body)) }
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode model request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		recordRequest()
		if mode == directLifecycleModelHoldOnCancel && request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-hold\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":null}]}\n\n", directLifecycleResponse)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			startedOnce.Do(func() { close(started) })
			// The reply is on the wire; the handler outlives the caller's
			// cancellation until the test lets it go. The wait is bounded so a
			// test that fails before releasing cannot wedge the run's suite
			// behind a handler that never returns.
			<-r.Context().Done()
			cancelledOnce.Do(func() { close(cancelled) })
			select {
			case <-release:
			case <-time.After(30 * time.Second):
			}
			return
		}
		if mode == directLifecycleModelBlock && request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-block\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			startedOnce.Do(func() { close(started) })
			<-r.Context().Done()
			return
		}
		if mode == directLifecycleModelSuccess && request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-direct-lifecycle\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":null}]}\n\n", directLifecycleResponse)
			_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-direct-lifecycle\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if mode == directLifecycleModelFailure {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"message":"private provider failure"}}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "chatcmpl-direct-lifecycle",
			"model": "direct-lifecycle-model",
			"choices": []map[string]any{{
				"index":         0,
				"finish_reason": "stop",
				"message": map[string]any{
					"role":    "assistant",
					"content": directLifecycleResponse,
				},
			}},
		})
	}))
	// The held handler is released before the server is closed, so a test that
	// failed mid-run cannot leave Close waiting on a handler that never
	// returns.
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		server.Close()
	})

	provider := agenttest.ProviderRow(
		t,
		directLifecycleProviderID,
		string(models.ClientTypeOpenAICompletions),
		true,
	)
	provider.Config, _ = json.Marshal(map[string]any{
		"api_key":  "test-key",
		"base_url": server.URL,
	})
	model := agenttest.ModelRow(
		t,
		directLifecycleModelID,
		"direct-lifecycle-model",
		provider.ID,
		models.ModelTypeChat,
		true,
	)
	model.Config = []byte(`{"context_window":128000}`)
	queries := &directLifecycleQueries{
		ModelQueries: agenttest.ModelQueries{
			Models:   map[string]sqlc.Model{model.ModelID: model},
			Provider: provider,
		},
		modelID: model.ID,
	}
	logger := slog.New(slog.DiscardHandler)
	streamTransport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("httptest transport = %T, want *http.Transport", server.Client().Transport)
	}
	lifecycles := &synchronizedLifecycleStore{}
	messages := &agenttest.RecordingMessages{}
	runtime := &agenttest.LifecycleAdmitter{Admission: lifecycleTestAdmission(), Fenced: true}
	httpClient := &http.Client{Transport: streamTransport}
	setup := directLifecycleSetup{deps: Deps{
		Logger:   logger,
		Agent:    native.New(native.Deps{Logger: logger}),
		Models:   models.NewService(logger, queries),
		Queries:  queries,
		Messages: messages,
		Settings: settings.NewService(logger, queries, nil, nil),
	}}
	setup.opts = []testOption{
		withLifecycleRecorder(lifecycles),
		withTurnAdmitter(runtime),
		withRunconfig(func(d *runconfig.Deps) { d.HTTPClient = httpClient }),
	}
	for _, option := range options {
		option(&setup)
	}
	var effective dbstore.Queries = queries
	for _, wrap := range setup.queries {
		effective = wrap(effective)
	}
	setup.deps.Queries = effective
	service := newTestService(t, setup.deps, setup.opts...)
	// The session runtime tells the lifecycle recorder of every run it ends, so
	// the snapshot a fenced run staged is written; the stand-in does the same.
	runtime.Observer = service.lifecycle.ReconcileTerminal
	return directLifecycleFixture{
		service:     service,
		queries:     queries,
		httpClient:  httpClient,
		lifecycles:  lifecycles,
		messages:    messages,
		runtime:     runtime,
		started:     started,
		cancelled:   cancelled,
		release:     release,
		releaseOnce: releaseOnce,
		requests:    requests,
	}
}

func assertDirectLifecycle(
	t *testing.T,
	store *synchronizedLifecycleStore,
	wantRunID, wantStatus, wantAssistantID string,
) {
	t.Helper()
	creates := store.creates()
	if len(creates) != 1 {
		t.Fatalf("lifecycle creates = %d, want 1", len(creates))
	}
	row := creates[0]
	if got := runreq.PGUUIDString(row.RunID); got != wantRunID {
		t.Fatalf("lifecycle run ID = %q, want %q", got, wantRunID)
	}
	if row.Status != wantStatus {
		t.Fatalf("lifecycle status = %q, want %q", row.Status, wantStatus)
	}
	if bytes.Contains(row.Snapshot, []byte(directLifecyclePrompt)) ||
		bytes.Contains(row.Snapshot, []byte(directLifecycleResponse)) {
		t.Fatalf("content-light snapshot leaked run content: %s", row.Snapshot)
	}
	var snapshot contextfrag.LifecycleSnapshot
	if err := json.Unmarshal(row.Snapshot, &snapshot); err != nil {
		t.Fatalf("decode lifecycle snapshot: %v", err)
	}
	if snapshot.Version != contextfrag.LifecycleSnapshotVersion || snapshot.View == "" || snapshot.AssistantMessageID != wantAssistantID {
		t.Fatalf("lifecycle snapshot = %#v, want authoritative version 1 and assistant %q", snapshot, wantAssistantID)
	}
}

const testACPUserInputOwnerID = "owner-user"

// scriptedFailureTransport answers the first streaming model requests with
// the scripted HTTP statuses and passes every later request to the real test
// provider. A 503 is retryable in the native runtime and a 400 is not, so the
// event order comes from production code: a single 503 gives
// EventRetry -> AgentEnd, and 503 then 400 gives
// EventRetry -> EventError -> AgentAbort.
type scriptedFailureTransport struct {
	base     http.RoundTripper
	statuses []int
	calls    atomic.Int32
}

func (t *scriptedFailureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	call := int(t.calls.Add(1))
	if call > len(t.statuses) {
		return t.base.RoundTrip(r)
	}
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}
	status := t.statuses[call-1]
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"` + http.StatusText(status) + `"}}`)),
		Request:    r,
	}, nil
}

// noDecisionQueries reports that the run has no pending decisions.
type noDecisionQueries struct {
	*directLifecycleQueries
	noDecisionReads
}

// proposalRecordingLedger records every finish proposal the manager makes, in
// order, so the test can show which outcome reached the ledger first.
type proposalRecordingLedger struct {
	*sessionledger.Store
	mu        sync.Mutex
	proposals [][2]string
	// failFirst makes the first proposal write fail, as a transient database
	// error would, so the terminal event defers the outcome to finish.
	failFirst bool
}

func (l *proposalRecordingLedger) PrepareFinish(ctx context.Context, params ledger.PrepareFinishParams) (ledger.Run, bool, error) {
	l.mu.Lock()
	l.proposals = append(l.proposals, [2]string{string(params.State), params.ErrorCode})
	failFirst := l.failFirst && len(l.proposals) == 1
	l.mu.Unlock()
	if failFirst {
		return ledger.Run{}, false, errors.New("proposal write temporarily unavailable")
	}
	return l.Store.PrepareFinish(ctx, params)
}

func (l *proposalRecordingLedger) snapshot() [][2]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([][2]string(nil), l.proposals...)
}

// nativeTurnFixture is a direct lifecycle service on a real session runtime,
// whose ledger records every finish proposal.
type nativeTurnFixture struct {
	directLifecycleFixture
	runs *proposalRecordingLedger
}

// newScriptedFailureFixture answers the model requests with statuses in order,
// then with the fixture's own reply. A nil runs is a recording ledger of its own
// and a nil logger discards the service's records.
func newScriptedFailureFixture(t *testing.T, runs *proposalRecordingLedger, logger *slog.Logger, statuses ...int) (nativeTurnFixture, *scriptedFailureTransport) {
	t.Helper()
	if runs == nil {
		runs = &proposalRecordingLedger{Store: sessionledger.New()}
	}
	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID: "owner-retry-turn", OwnerLeaseTTL: time.Minute, Ledger: runs, Fence: agenttest.NoopFence{},
	})
	t.Cleanup(func() { _ = manager.Close() })
	// The transport wraps the fixture's own, which exists only once the fixture
	// does, so the client is filled in by the runconfig option below.
	transport := &scriptedFailureTransport{statuses: statuses}
	client := &http.Client{Transport: transport}
	options := []directLifecycleOption{
		withFixtureRuntime(manager),
		withFixtureOptions(withRunconfig(func(d *runconfig.Deps) { d.HTTPClient = client })),
	}
	if logger != nil {
		options = append(options, withFixtureDeps(func(d *Deps) { d.Logger = logger }))
	}
	fixture := newDirectLifecycleFixture(t, directLifecycleModelSuccess, options...)
	transport.base = fixture.httpClient.Transport
	return nativeTurnFixture{directLifecycleFixture: fixture, runs: runs}, transport
}

func eventTypes(t *testing.T, payloads []json.RawMessage) []string {
	t.Helper()
	types := make([]string, 0, len(payloads))
	for _, payload := range payloads {
		var env struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload, &env); err != nil {
			t.Fatalf("decode event %s: %v", payload, err)
		}
		types = append(types, env.Type)
	}
	return types
}

// nativeTurnRun is what one native turn through the turn port produced. It is
// named apart from the turn contract's own delivery end, which the driver owns.
type nativeTurnRun struct {
	types     []string
	errCodes  []string
	errs      []error
	proposals [][2]string
	ledger    [3]string
}

// runNativeTurn drives StartTurn against a real session runtime
// manager and an in-memory ledger that records every finish proposal.
func runNativeTurn(t *testing.T, fixture nativeTurnFixture) nativeTurnRun {
	t.Helper()
	runs := fixture.runs
	handle, err := fixture.service.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion:        1,
		TeamID:               "retry-team",
		Mode:                 turn.ModeChat,
		BotID:                agenttest.LifecycleBotID,
		ChatID:               agenttest.LifecycleBotID,
		ThreadID:             agenttest.LifecycleSessionID,
		Query:                directLifecyclePrompt,
		UserMessagePersisted: true,
	})
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	var payloads []json.RawMessage
	var out nativeTurnRun
	done := make(chan struct{})
	go func() {
		defer close(done)
		for event := range handle.Events() {
			payloads = append(payloads, event.Payload)
		}
		for err := range handle.Errs() {
			out.errCodes = append(out.errCodes, string(apperror.CodeOf(err)))
			out.errs = append(out.errs, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("turn did not finish")
	}
	out.types = eventTypes(t, payloads)
	out.proposals = runs.snapshot()
	run, err := runs.Get(context.Background(), handle.RunID())
	if err != nil {
		t.Fatalf("load ledger run: %v", err)
	}
	out.ledger = [3]string{string(run.State), run.ErrorCode, run.ErrorMessage}
	return out
}

// assertOrdered checks that want appears in got as a subsequence.
func assertOrdered(t *testing.T, got []string, want ...string) {
	t.Helper()
	i := 0
	for _, value := range got {
		if i < len(want) && value == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Fatalf("event types = %q, want %q in order", got, want)
	}
}

func newTurnTestService(t *testing.T, opts ...testOption) *Service {
	t.Helper()
	service, _ := newAdmittedTurnTestService(t, opts...)
	return service
}

func newAdmittedTurnTestService(t *testing.T, opts ...testOption) (*Service, *agenttest.ScriptedAdmitter) {
	t.Helper()
	admitter := agenttest.NewScriptedAdmitter()
	service := newTestService(t, Deps{}, append([]testOption{
		withTurnAdmitter(admitter),
		withDrive(func(d *drive.Deps) { d.Events = agenttest.EventFunc(admitter.PublishAgentEvent) }),
	}, opts...)...)
	return service, admitter
}

// chatTurnFixture is the chat entries' test service: a real application service
// reaching a scripted model provider, with a scripted admission gate so a test
// can read the run's terminal write and what was published to the projection.
// Chat entries resolve through the whole application path, so a test that
// asserts what a chat run did needs the real one.
type chatTurnFixture struct {
	fixture  directLifecycleFixture
	admitter *agenttest.ScriptedAdmitter
}

func newChatTurnFixture(t *testing.T, mode directLifecycleModelMode, options ...directLifecycleOption) chatTurnFixture {
	t.Helper()
	admitter := agenttest.NewScriptedAdmitter()
	admitter.Fenced = true
	// The composition root compiles the run's context fragments into the
	// provider-facing request there; without it the routing a run assembled
	// never reaches the model, and a test could not tell a lost route from an
	// absent one.
	logger := slog.New(slog.DiscardHandler)
	applier := contextview.ProviderRunConfigApplier(logger)
	base := []directLifecycleOption{
		withNoDecisionQueries(),
		withFixtureOptions(
			withTurnAdmitter(admitter),
			withDrive(func(d *drive.Deps) { d.Events = agenttest.EventFunc(admitter.PublishAgentEvent) }),
		),
		withFixtureDeps(func(d *Deps) {
			d.Agent = native.New(native.Deps{
				Logger:             logger,
				ContextViewApplier: applier,
			})
			// The chat path reads the session for the facts a run executes under,
			// the same way a hosted service does.
			d.Sessions = &agenttest.Sessions{
				GetFn: func(_ context.Context, sessionID string) (sessionpkg.Thread, error) {
					return sessionpkg.Thread{
						ID: sessionID, BotID: agenttest.LifecycleBotID, Type: sessionpkg.TypeChat,
						SessionMode: sessionpkg.TypeChat, RuntimeType: sessionpkg.RuntimeModel,
					}, nil
				},
				UpdateMetadataFn: func(_ context.Context, sessionID string, _ map[string]any) (sessionpkg.Thread, error) {
					return sessionpkg.Thread{ID: sessionID}, nil
				},
			}
		}),
	}
	fixture := newDirectLifecycleFixture(t, mode, append(base, options...)...)
	admitter.Observer = fixture.service.lifecycle.ReconcileTerminal
	return chatTurnFixture{fixture: fixture, admitter: admitter}
}

// start runs one chat turn through the turn port and returns its handle.
func (f chatTurnFixture) start(t *testing.T, cmd turn.StartTurnCommand) turn.RunHandle {
	t.Helper()
	if cmd.BotID == "" {
		cmd.BotID = agenttest.LifecycleBotID
	}
	if cmd.ThreadID == "" {
		cmd.ThreadID = agenttest.LifecycleSessionID
	}
	if cmd.ChatID == "" {
		cmd.ChatID = agenttest.LifecycleBotID
	}
	if cmd.Query == "" {
		cmd.Query = directLifecyclePrompt
	}
	handle, err := f.fixture.service.StartTurn(context.Background(), cmd)
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	return handle
}

// releaseModel lets a held model call return. It is idempotent, so a test may
// call it explicitly and the fixture's cleanup may call it again.
func (f chatTurnFixture) releaseModel(t *testing.T) {
	t.Helper()
	f.fixture.releaseOnce.Do(func() { close(f.fixture.release) })
}

// awaitModelRequest waits for the fixture's model endpoint to receive its
// first request, which is how a test reads a run that is still executing.
func (f chatTurnFixture) awaitModelRequest(t *testing.T) {
	t.Helper()
	select {
	case <-f.fixture.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never reached the model provider")
	}
}

// stopRun cancels the run and waits for the cancellation to reach the model
// request, so a test that asserts on the stopping window is not racing the
// cancellation it just asked for.
func (f chatTurnFixture) stopRun(t *testing.T, handle turn.RunHandle) {
	t.Helper()
	handle.Cancel()
	select {
	case <-f.fixture.cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling the run did not reach the model request")
	}
}

func recordTurnSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(previous) })
	return recorder
}

func turnSpan(t *testing.T, recorder *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, span := range recorder.Ended() {
			if span.Name() == "agent.turn" {
				return span
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no agent.turn span ended within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func spanAttr(span sdktrace.ReadOnlySpan, key string) attribute.Value {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value
		}
	}
	return attribute.Value{}
}

// awaitEntryReturned waits for an entry that runs on its caller's goroutine to
// return, which it does once its run has ended.
func awaitEntryReturned(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the entry did not return after its run")
	}
}
