package application

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
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/settings"
)

const (
	directLifecycleModelID    = "44444444-4444-4444-8444-444444444444"
	directLifecycleProviderID = "55555555-5555-4555-8555-555555555555"
	directLifecyclePrompt     = "PRIVATE_DIRECT_LIFECYCLE_PROMPT"
	directLifecycleResponse   = "PRIVATE_DIRECT_LIFECYCLE_RESPONSE"
)

type directLifecycleQueries struct {
	modelSelectionFakeQueries
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
	service    *Service
	lifecycles *synchronizedLifecycleStore
	messages   *recordingMessageService
	runtime    *lifecycleTurnAdmitter
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
	store recordingContextLifecycleStore
}

func (s *synchronizedLifecycleStore) CreateContextLifecycle(
	ctx context.Context,
	arg sqlc.CreateContextLifecycleParams,
) (sqlc.CreateContextLifecycleRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store.existing != nil {
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
	if err == nil && s.store.existing != nil {
		s.store.existing.Snapshot = append([]byte(nil), arg.Snapshot...)
		for i := range s.store.creates {
			if s.store.creates[i].RunID == arg.RunID {
				s.store.creates[i].Snapshot = append([]byte(nil), arg.Snapshot...)
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

func (s *synchronizedLifecycleStore) creates() []sqlc.CreateContextLifecycleParams {
	s.mu.Lock()
	defer s.mu.Unlock()
	creates := make([]sqlc.CreateContextLifecycleParams, len(s.store.creates))
	copy(creates, s.store.creates)
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

func newDirectLifecycleFixture(t *testing.T, mode directLifecycleModelMode) directLifecycleFixture {
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

	provider := modelSelectionProviderRow(
		t,
		directLifecycleProviderID,
		string(models.ClientTypeOpenAICompletions),
		true,
	)
	provider.Config, _ = json.Marshal(map[string]any{
		"api_key":  "test-key",
		"base_url": server.URL,
	})
	model := modelSelectionModelRow(
		t,
		directLifecycleModelID,
		"direct-lifecycle-model",
		provider.ID,
		models.ModelTypeChat,
		true,
	)
	model.Config = []byte(`{"context_window":128000}`)
	queries := &directLifecycleQueries{
		modelSelectionFakeQueries: modelSelectionFakeQueries{
			models:   map[string]sqlc.Model{model.ModelID: model},
			provider: provider,
		},
		modelID: model.ID,
	}
	logger := slog.New(slog.DiscardHandler)
	streamTransport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("httptest transport = %T, want *http.Transport", server.Client().Transport)
	}
	lifecycles := &synchronizedLifecycleStore{}
	messages := &recordingMessageService{}
	runtime := &lifecycleTurnAdmitter{admission: lifecycleTestAdmission()}
	service := &Service{
		agent:             native.New(native.Deps{Logger: logger}),
		modelsService:     models.NewService(logger, queries),
		queries:           queries,
		contextLifecycles: lifecycles,
		messageService:    messages,
		settingsService:   settings.NewService(logger, queries, nil, nil),
		sessionRuntime:    runtime,
		streamHTTPClient:  &http.Client{Transport: streamTransport},
		logger:            logger,
	}
	return directLifecycleFixture{
		service:     service,
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
	if got := pgUUIDString(row.RunID); got != wantRunID {
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

func startDirectLifecycleTurn(t *testing.T, fixture directLifecycleFixture) {
	t.Helper()
	handle, err := fixture.service.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion:        1,
		TeamID:               "direct-lifecycle-team",
		Mode:                 turn.ModeChat,
		BotID:                lifecycleTestBotID,
		ChatID:               lifecycleTestBotID,
		ThreadID:             lifecycleTestSessionID,
		Query:                directLifecyclePrompt,
		UserMessagePersisted: true,
	})
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	for range handle.Events() {
	}
	for range handle.Errs() {
	}
}

func TestAdmittedStreamPersistsCompletedLifecycleAfterAssistant(t *testing.T) {
	fixture := newDirectLifecycleFixture(t, directLifecycleModelSuccess)

	startDirectLifecycleTurn(t, fixture)

	if len(fixture.messages.persisted) == 0 {
		t.Fatal("the run did not persist its assistant response")
	}
	runID := fixture.messages.persisted[len(fixture.messages.persisted)-1].RunID
	if runID != lifecycleTestRunID {
		t.Fatalf("persisted run ID = %q, want the admitted run %q", runID, lifecycleTestRunID)
	}
	assertDirectLifecycle(t, fixture.lifecycles, runID, contextLifecycleStatusCompleted, "message-id")
}

func TestAdmittedStreamProviderFailurePersistsFailedProviderLifecycle(t *testing.T) {
	fixture := newDirectLifecycleFixture(t, directLifecycleModelFailure)

	startDirectLifecycleTurn(t, fixture)

	creates := fixture.lifecycles.creates()
	if len(creates) != 1 {
		t.Fatalf("lifecycle creates = %d, want 1", len(creates))
	}
	assertDirectLifecycle(t, fixture.lifecycles, lifecycleTestRunID, contextLifecycleStatusFailedProvider, "message-id")
}

func TestAdmittedStreamCancellationPersistsAbortedLifecycle(t *testing.T) {
	fixture := newDirectLifecycleFixture(t, directLifecycleModelBlock)

	handle, err := fixture.service.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion:        1,
		TeamID:               "direct-lifecycle-team",
		Mode:                 turn.ModeChat,
		BotID:                lifecycleTestBotID,
		ChatID:               lifecycleTestBotID,
		ThreadID:             lifecycleTestSessionID,
		Query:                directLifecyclePrompt,
		UserMessagePersisted: true,
	})
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	select {
	case <-fixture.started:
	case <-time.After(3 * time.Second):
		t.Fatal("streaming provider did not start")
	}
	handle.Cancel()
	done := make(chan struct{})
	go func() {
		for range handle.Events() {
		}
		for range handle.Errs() {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled admitted stream did not finish")
	}

	assertDirectLifecycle(t, fixture.lifecycles, lifecycleTestRunID, contextLifecycleStatusAborted, "")
	if len(fixture.runtime.finishes) != 1 || fixture.runtime.finishes[0].status != sessionruntime.RunStatusAborted {
		t.Fatalf("runtime finishes = %#v, want one aborted finish", fixture.runtime.finishes)
	}
}

func TestAdmittedStreamWithholdsTerminalWhenHistoryPersistenceFails(t *testing.T) {
	fixture := newDirectLifecycleFixture(t, directLifecycleModelSuccess)
	persistErr := errors.New("history unavailable")
	fixture.messages.roundPersistErr = persistErr

	var publishedMu sync.Mutex
	var published []native.StreamEvent
	fixture.service.publishTurnEvent = func(_ context.Context, _ sessionruntime.RunHandle, event native.StreamEvent) error {
		publishedMu.Lock()
		defer publishedMu.Unlock()
		published = append(published, event)
		return nil
	}

	handle, err := fixture.service.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion:        1,
		TeamID:               "direct-lifecycle-team",
		Mode:                 turn.ModeChat,
		BotID:                lifecycleTestBotID,
		ChatID:               lifecycleTestBotID,
		ThreadID:             lifecycleTestSessionID,
		Query:                directLifecyclePrompt,
		UserMessagePersisted: true,
	})
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	for range handle.Events() {
	}
	for range handle.Errs() {
	}

	if len(fixture.runtime.finishes) != 1 || fixture.runtime.finishes[0].status != sessionruntime.RunStatusErrored {
		t.Fatalf("runtime finishes = %#v, want one errored finish", fixture.runtime.finishes)
	}
	publishedMu.Lock()
	defer publishedMu.Unlock()
	for _, event := range published {
		if event.IsTerminal() {
			t.Fatalf("terminal event %q was published after failed history persistence", event.Type)
		}
	}
}

// gap 2 regression: a caller that goes away while its run is streaming named
// the ending a failure before. The caller's context ending is this run's stop
// signal, so the run must end stopped — not errored with a code nobody raised.
func TestAdmittedStreamCallerContextEndingIsAStop(t *testing.T) {
	fixture := newDirectLifecycleFixture(t, directLifecycleModelBlock)

	caller, stopCaller := context.WithCancel(context.Background())
	handle, err := fixture.service.StartTurn(caller, turn.StartTurnCommand{
		SchemaVersion:        1,
		TeamID:               "direct-lifecycle-team",
		Mode:                 turn.ModeChat,
		BotID:                lifecycleTestBotID,
		ChatID:               lifecycleTestBotID,
		ThreadID:             lifecycleTestSessionID,
		Query:                directLifecyclePrompt,
		UserMessagePersisted: true,
	})
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	select {
	case <-fixture.started:
	case <-time.After(3 * time.Second):
		t.Fatal("streaming provider did not start")
	}

	// The caller's own context goes away, as it does when an inbound consumer
	// disconnects. It never touches this run's stop marker directly.
	stopCaller()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range handle.Events() {
		}
		for range handle.Errs() {
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not finish after its caller went away")
	}

	if len(fixture.runtime.finishes) != 1 || fixture.runtime.finishes[0].status != sessionruntime.RunStatusAborted {
		t.Fatalf("runtime finishes = %#v, want one aborted finish", fixture.runtime.finishes)
	}
	if fixture.runtime.finishes[0].message != "" {
		t.Fatalf("finish message = %q, want none for a stop", fixture.runtime.finishes[0].message)
	}
	assertDirectLifecycle(t, fixture.lifecycles, lifecycleTestRunID, contextLifecycleStatusAborted, "")
}
