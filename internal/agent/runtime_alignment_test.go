package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/runreq"
	acpagent "github.com/felinics/memoh/internal/agent/runtime/acp"
	acpclient "github.com/felinics/memoh/internal/agent/runtime/acp/client"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

type failNthPersistMessages struct {
	*agenttest.RecordingMessages
	failAt int
	calls  int
	err    error
}

func (s *failNthPersistMessages) Persist(
	ctx context.Context,
	input messagepkg.PersistInput,
) (messagepkg.Message, error) {
	s.calls++
	if s.calls == s.failAt {
		return messagepkg.Message{}, s.err
	}
	return s.RecordingMessages.Persist(ctx, input)
}

// PersistRound mirrors the per-message failure for the atomic round path: a
// round whose write would cross failAt fails as one transaction.
func (s *failNthPersistMessages) PersistRound(
	ctx context.Context,
	inputs []messagepkg.PersistInput,
	options messagepkg.RoundPersistenceOptions,
) ([]messagepkg.Message, bool, error) {
	if s.calls+len(inputs) >= s.failAt {
		s.calls = s.failAt
		return nil, true, s.err
	}
	s.calls += len(inputs)
	return s.RecordingMessages.PersistRound(ctx, inputs, options)
}

// failingPromptDriver is an External Agent runtime that cannot run the turn.
type failingPromptDriver struct{ err error }

func (*failingPromptDriver) RuntimeType() string { return sessionpkg.RuntimeACPAgent }

func (d *failingPromptDriver) Prompt(context.Context, external.PromptInput) (external.PromptResult, error) {
	return external.PromptResult{}, d.err
}

// A prompt the External Agent runtime cannot complete fails the run under the
// runtime's own code, and the provider's text stays out of everything the run
// delivers: the failure is named by its catalog code, never by the sentence the
// runtime returned.
func TestACPGenericPromptFailurePublishesSanitizedErroredTerminal(t *testing.T) {
	const privateDetail = "PRIVATE_ACP_PROVIDER_FAILURE"

	service, _ := newDrivePromptFixture(t, withDrivers(&failingPromptDriver{err: errors.New(privateDetail)}))

	handle, err := service.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion: 1,
		TeamID:        "team-1",
		Mode:          turn.ModeChat,
		BotID:         agenttest.LifecycleBotID,
		ThreadID:      agenttest.LifecycleSessionID,
	})
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	var delivered []string
	var abort string
	deadline := time.After(10 * time.Second)
	for eventCh, errCh := handle.Events(), handle.Errs(); eventCh != nil || errCh != nil; {
		select {
		case event, ok := <-eventCh:
			if !ok {
				eventCh = nil
				continue
			}
			delivered = append(delivered, string(event.Payload))
			if event.Kind == string(native.EventAgentAbort) {
				abort = string(event.Payload)
			}
		case _, ok := <-errCh:
			if !ok {
				errCh = nil
				continue
			}
		case <-deadline:
			t.Fatal("run did not end")
		}
	}
	for _, payload := range delivered {
		if strings.Contains(payload, privateDetail) {
			t.Fatalf("delivered an event carrying the provider's text: %s", payload)
		}
	}
	// The failure reaches the caller as the runtime's own abort, carrying the
	// catalog code and no provider text.
	if abort == "" {
		t.Fatalf("delivered events = %v, want the runtime's failed abort", delivered)
	}
	if !strings.Contains(abort, string(apperror.CodeRuntimePromptFailed)) {
		t.Fatalf("abort = %s, want code %s", abort, apperror.CodeRuntimePromptFailed)
	}
}

func TestACPPersistFailureWithholdsTerminalAndPublishesSanitizedError(t *testing.T) {
	privateErr := errors.New("PRIVATE_ACP_PERSIST_FAILURE")
	messages := &failNthPersistMessages{
		RecordingMessages: &agenttest.RecordingMessages{},
		failAt:            2,
		err:               privateErr,
	}
	lifecycles := &agenttest.RecordingLifecycleStore{}
	service := newACPLifecycleService(
		t,
		&recordingACPPrompter{result: acpclient.PromptResult{Text: "done", StopReason: "end_turn"}},
		messages,
		lifecycles,
	)
	eventCh := make(chan runreq.WSStreamEvent, 16)

	if _, err := service.streamACPAgentWS(context.Background(), runreq.ChatRequest{
		BotID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID,
		RunID: agenttest.LifecycleRunID, Query: "inspect",
	}, eventCh, make(chan struct{})); err != nil {
		t.Fatalf("streamACPAgentWS() error = %v", err)
	}
	events := drainAgentEvents(t, eventCh)
	assertACPCommitFailureEvents(t, events, string(apperror.CodeSessionHistoryInconsistent), privateErr.Error())
	row, _ := requireACPLifecycle(t, lifecycles, agenttest.LifecycleRunID, lifecycle.StatusFailedProvider)
	if !row.ErrorCode.Valid || row.ErrorCode.String != string(apperror.CodeSessionHistoryInconsistent) {
		t.Fatalf("lifecycle error code = %#v, want %q", row.ErrorCode, apperror.CodeSessionHistoryInconsistent)
	}
	// The eagerly persisted leading user message is deliberately KEPT on a
	// definite round rollback: the user watched their message send, a visible
	// message must never vanish, and keeping it can never destroy a committed
	// turn the way a misclassified rollback deletion would. This pin exists
	// because reviewers have proposed both directions; the keep decision is
	// intentional, not an omission.
	for _, batch := range messages.Deleted {
		for _, id := range batch {
			if id == "message-id" {
				t.Fatalf("round rollback deleted the leading user message: %#v", messages.Deleted)
			}
		}
	}
}

func TestACPExplicitCancellationDoesNotPublishProviderError(t *testing.T) {
	started := make(chan struct{})
	pool := &recordingACPPrompter{promptFn: func(ctx context.Context, _ acpagent.PromptInput) (acpclient.PromptResult, error) {
		close(started)
		<-ctx.Done()
		return acpclient.PromptResult{}, context.Cause(ctx)
	}}
	service := newACPLifecycleService(t, pool, &agenttest.RecordingMessages{}, &agenttest.RecordingLifecycleStore{})
	eventCh := make(chan runreq.WSStreamEvent, 16)
	abortCh := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ignoreOutcome(service.streamACPAgentWS(context.Background(), runreq.ChatRequest{
			BotID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID,
			RunID: agenttest.LifecycleRunID, Query: "inspect",
		}, eventCh, abortCh))
	}()
	<-started
	close(abortCh)
	if err := <-done; err != nil {
		t.Fatalf("streamACPAgentWS() error = %v", err)
	}
	events := drainAgentEvents(t, eventCh)
	if containsStreamEvent(events, native.EventError) || containsStreamEvent(events, native.EventEnd) {
		t.Fatalf("events = %#v, explicit cancellation must not look completed or provider-failed", events)
	}
}

// assertACPCommitFailureEvents checks what the run published when its history
// write failed: the failure under its stable code and no terminal, which a
// round that did not land must not announce, and none of the private cause.
func assertACPCommitFailureEvents(t *testing.T, events []native.StreamEvent, wantCode, privateDetail string) {
	t.Helper()
	failures := 0
	for _, event := range events {
		switch {
		case event.Type == native.EventError:
			failures++
			if event.Code != wantCode {
				t.Fatalf("runtime error code = %q, want %q", event.Code, wantCode)
			}
		case event.IsTerminal():
			t.Fatalf("events = %#v, a round that did not land published terminal %q", events, event.Type)
		}
	}
	if failures != 1 {
		t.Fatalf("events = %#v, want one failure event", events)
	}
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), privateDetail) {
		t.Fatalf("runtime events leaked private diagnostic: %s", raw)
	}
}

// reconcileOutcomeQueries fakes the commit-unknown reconciliation store: it
// answers LockSessionForCommitReconciliation + GetRuntimeRoundOutcome inside a
// pass-through transaction, can fail a configurable number of attempts first,
// and signals when a projection sweep runs.
type reconcileOutcomeQueries struct {
	dbstore.Queries
	mu               sync.Mutex
	outcome          string // "" means no committed round (proven rollback)
	failFirst        int    // reconcile attempts to fail before answering
	projectionsSwept chan struct{}
}

func (*reconcileOutcomeQueries) SupportsTransactions() bool { return true }

func (*reconcileOutcomeQueries) ListCompactionArtifactLineageBySession(context.Context, pgtype.UUID) ([]sqlc.BotHistoryMessageCompact, error) {
	return nil, nil
}

func (q *reconcileOutcomeQueries) InTx(_ context.Context, fn func(dbstore.Queries) error) error {
	q.mu.Lock()
	if q.failFirst > 0 {
		q.failFirst--
		q.mu.Unlock()
		return errors.New("reconciliation store temporarily unavailable")
	}
	q.mu.Unlock()
	return fn(q)
}

// GetBotByID satisfies the incidental timezone lookup on this path.
func (*reconcileOutcomeQueries) GetBotByID(context.Context, pgtype.UUID) (sqlc.GetBotByIDRow, error) {
	return sqlc.GetBotByIDRow{}, pgx.ErrNoRows
}

func (*reconcileOutcomeQueries) LockSessionForCommitReconciliation(
	context.Context, sqlc.LockSessionForCommitReconciliationParams,
) (pgtype.UUID, error) {
	return pgtype.UUID{}, nil
}

func (q *reconcileOutcomeQueries) GetRuntimeRoundOutcome(context.Context, sqlc.GetRuntimeRoundOutcomeParams) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.outcome == "" {
		return "", pgx.ErrNoRows
	}
	return q.outcome, nil
}

func (q *reconcileOutcomeQueries) DeleteRuntimeDecisionProjectionsByRun(
	context.Context, sqlc.DeleteRuntimeDecisionProjectionsByRunParams,
) (int64, error) {
	select {
	case q.projectionsSwept <- struct{}{}:
	default:
	}
	return 0, nil
}

// TestACPCommitUnknownResolutionMatrix pins the three-way commit-unknown
// resolution contract of resolveRuntimeRoundPersistFailure through the
// completed-turn path, plus the incomplete-abort rollback path. Two invariants
// hold in every cell: the eagerly persisted leading user message is never
// deleted, and the warm runtime is only discarded on a PROVEN rollback of a
// completed turn.
func TestACPCommitUnknownResolutionMatrix(t *testing.T) {
	assertUserMessageKept := func(t *testing.T, messages *agenttest.RecordingMessages) {
		t.Helper()
		for _, batch := range messages.Deleted {
			for _, id := range batch {
				if id == "message-id" {
					t.Fatalf("leading user message was deleted: %#v", messages.Deleted)
				}
			}
		}
	}

	t.Run("committed", func(t *testing.T) {
		messages := &agenttest.RecordingMessages{RoundPersistErr: db.ErrCommitOutcomeUnknown}
		pool := &recordingACPPrompter{result: acpclient.PromptResult{Text: "done", StopReason: "end_turn"}}
		service := newACPLifecycleService(t, pool, messages, &agenttest.RecordingLifecycleStore{}, withDeps(func(d *Deps) { d.Queries = &reconcileOutcomeQueries{outcome: "succeeded"} }))
		eventCh := make(chan runreq.WSStreamEvent, 16)

		if _, err := service.streamACPAgentWS(context.Background(), runreq.ChatRequest{
			BotID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID,
			RunID: agenttest.LifecycleRunID, Query: "inspect",
		}, eventCh, make(chan struct{})); err != nil {
			t.Fatalf("streamACPAgentWS() error = %v", err)
		}
		events := drainAgentEvents(t, eventCh)
		if !containsStreamEvent(events, native.EventEnd) {
			t.Fatalf("proven-committed round did not terminate as completed: %#v", events)
		}
		if len(pool.closed) != 0 {
			t.Fatalf("proven-committed round closed the runtime: %v", pool.closed)
		}
		assertUserMessageKept(t, messages)
	})

	t.Run("rolled back", func(t *testing.T) {
		messages := &agenttest.RecordingMessages{RoundPersistErr: db.ErrCommitOutcomeUnknown}
		pool := &recordingACPPrompter{result: acpclient.PromptResult{Text: "done", StopReason: "end_turn"}}
		service := newACPLifecycleService(t, pool, messages, &agenttest.RecordingLifecycleStore{}, withDeps(func(d *Deps) { d.Queries = &reconcileOutcomeQueries{outcome: ""} }))
		eventCh := make(chan runreq.WSStreamEvent, 16)

		outcome, err := service.streamACPAgentWS(context.Background(), runreq.ChatRequest{
			BotID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID,
			RunID: agenttest.LifecycleRunID, Query: "inspect",
		}, eventCh, make(chan struct{}))
		if err != nil || apperror.CodeOf(outcome.Cause) != apperror.CodeSessionHistoryInconsistent {
			t.Fatalf("streamACPAgentWS() = %v, %v, want the run to fail with %s", outcome.Cause, err, apperror.CodeSessionHistoryInconsistent)
		}
		events := drainAgentEvents(t, eventCh)
		if !containsStreamEvent(events, native.EventError) || containsStreamEvent(events, native.EventEnd) {
			t.Fatalf("proven rollback of a completed turn must surface a sanitized error: %#v", events)
		}
		if len(pool.closed) != 1 || pool.closed[0] != agenttest.LifecycleSessionID {
			t.Fatalf("proven rollback must discard the diverged warm runtime once: %v", pool.closed)
		}
		assertUserMessageKept(t, messages)
	})

	t.Run("unresolved", func(t *testing.T) {
		messages := &agenttest.RecordingMessages{RoundPersistErr: db.ErrCommitOutcomeUnknown}
		pool := &recordingACPPrompter{result: acpclient.PromptResult{Text: "done", StopReason: "end_turn"}}
		queries := &reconcileOutcomeQueries{
			outcome:          "succeeded",
			failFirst:        1, // the in-turn reconcile fails; the background retry succeeds
			projectionsSwept: make(chan struct{}, 1),
		}
		service := newACPLifecycleService(t, pool, messages, &agenttest.RecordingLifecycleStore{}, withDeps(func(d *Deps) { d.Queries = queries }))
		eventCh := make(chan runreq.WSStreamEvent, 16)

		outcome, err := service.streamACPAgentWS(context.Background(), runreq.ChatRequest{
			BotID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID,
			RunID: agenttest.LifecycleRunID, Query: "inspect",
		}, eventCh, make(chan struct{}))
		if err != nil || apperror.CodeOf(outcome.Cause) != apperror.CodeSessionHistoryInconsistent {
			t.Fatalf("unresolved outcome must fail closed, got %v, %v", outcome.Cause, err)
		}
		if len(pool.closed) != 0 {
			t.Fatalf("unresolved outcome must not touch the runtime: %v", pool.closed)
		}
		assertUserMessageKept(t, messages)
		// The projections are swept, by the run's own cleanup or by the
		// reconciliation that resolves the round, whichever comes first.
		select {
		case <-queries.projectionsSwept:
		case <-time.After(5 * time.Second):
			t.Fatal("the decision projections were never swept")
		}
		assertUserMessageKept(t, messages)
	})

	t.Run("incomplete abort rollback keeps runtime", func(t *testing.T) {
		messages := &agenttest.RecordingMessages{RoundPersistErr: errors.New("definite rollback")}
		started := make(chan struct{})
		pool := &recordingACPPrompter{promptFn: func(ctx context.Context, _ acpagent.PromptInput) (acpclient.PromptResult, error) {
			close(started)
			<-ctx.Done()
			return acpclient.PromptResult{Text: "partial"}, context.Cause(ctx)
		}}
		service := newACPLifecycleService(t, pool, messages, &agenttest.RecordingLifecycleStore{}, withDeps(func(d *Deps) { d.Queries = &reconcileOutcomeQueries{} }))
		eventCh := make(chan runreq.WSStreamEvent, 16)
		abortCh := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- ignoreOutcome(service.streamACPAgentWS(context.Background(), runreq.ChatRequest{
				BotID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID,
				RunID: agenttest.LifecycleRunID, Query: "inspect",
			}, eventCh, abortCh))
		}()
		<-started
		close(abortCh)
		if err := <-done; err != nil {
			t.Fatalf("streamACPAgentWS() error = %v", err)
		}
		if len(pool.closed) != 0 {
			t.Fatalf("incomplete abort must keep the runtime (head never needed to move): %v", pool.closed)
		}
		assertUserMessageKept(t, messages)
	})
}
