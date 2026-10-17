package application

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
	"github.com/felinics/memoh/internal/runtimefence"
)

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

// newDriveHistoryFixture seeds one bot and session and wires the application's
// real stores over the test database.
func newDriveHistoryFixture(t *testing.T, mode directLifecycleModelMode) (chatTurnFixture, *pgxpool.Pool, string, string) {
	t.Helper()
	ctx := context.Background()
	pool := openTurnAdmissionPostgres(t, ctx)
	botID, sessionID := createTurnAdmissionFixture(t, ctx, pool)
	seedDirectLifecycleModel(t, ctx, pool)
	queries := postgresstore.NewQueriesWithPool(pool, dbsqlc.New(pool))
	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID:       "owner-drive-history",
		StateTTL:      time.Minute,
		OwnerLeaseTTL: time.Minute,
		Ledger:        ledger.NewPostgres(dbsqlc.New(pool), pool),
		Fence:         runtimefence.NewActivator(queries),
	})
	t.Cleanup(func() { _ = manager.Close() })

	fixture := newChatTurnFixture(t, mode)
	service := fixture.fixture.service
	service.bindSessionRuntime(manager)
	service.messageService = messagepkg.NewService(service.logger, queries)
	// The real store, with the two decision reads a finishing run makes
	// answered as "none": this fixture creates no decisions.
	service.queries = postgresNoDecisionQueries{queries}
	setAllowedTeam(service, "")
	return fixture, pool, botID, sessionID
}

// postgresNoDecisionQueries is the real PostgreSQL store with the run's
// pending-decision reads answered as none.
type postgresNoDecisionQueries struct{ *postgresstore.Queries }

func (postgresNoDecisionQueries) ListToolApprovalsByRun(context.Context, pgtype.UUID) ([]dbsqlc.ToolApprovalRequest, error) {
	return nil, nil
}

func (postgresNoDecisionQueries) ListUserInputsByRun(context.Context, pgtype.UUID) ([]dbsqlc.UserInputRequest, error) {
	return nil, nil
}

// historyRowsFor lists the conversation's rows newest first, as the history
// endpoint would.
func historyRowsFor(t *testing.T, fixture chatTurnFixture, sessionID string) []messagepkg.Message {
	t.Helper()
	svc, ok := fixture.fixture.service.messageService.(*messagepkg.DBService)
	if !ok {
		t.Fatalf("message service = %T, want the PostgreSQL store", fixture.fixture.service.messageService)
	}
	rows, err := svc.ListBySession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("list history: %v", err)
	}
	return rows
}

// An IM turn that fails in setup leaves the sender's message and a failure row:
// the input left the submitter, so the sender has to see why it was not
// answered, and the row is what the next turn reads.
func TestDriveHistoryIMSetupFailureWritesTheUserMessageAndRow(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	fixture, _, botID, sessionID := newDriveHistoryFixture(t, directLifecycleModelFailure)
	handle := fixture.start(t, turn.StartTurnCommand{
		TeamID: "team-1", BotID: botID, ChatID: botID, ThreadID: sessionID,
		Query: directLifecyclePrompt, UserVisibleText: directLifecyclePrompt,
		IdempotencyKey: "im-history-1",
	})
	collectEvents(t, handle)

	rows := historyRowsFor(t, fixture, sessionID)
	var sawUser, sawFailure bool
	for _, row := range rows {
		if row.Role == "user" {
			sawUser = true
		}
		if code, _ := row.Metadata[messagepkg.HistoryErrorCodeMetadataKey].(string); code != "" {
			sawFailure = true
		}
	}
	if !sawUser {
		t.Fatalf("history = %d rows, want the sender's own message", len(rows))
	}
	if !sawFailure {
		t.Fatalf("history = %d rows, want a failure row beside it", len(rows))
	}
}

// A resume writes no conversation history: nobody submitted the turn, and the
// input it continues is already in the session. The run's outcome lives in its
// record and the fire's log instead.
func TestDriveHistoryResumeWritesNoConversationRows(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	fixture, pool, botID, sessionID := newDriveHistoryFixture(t, directLifecycleModelSuccess)
	service := fixture.fixture.service
	ctx := context.Background()

	// The interrupted run the resume continues: it is what the ledger checks
	// the continuation against, and what the intent was saved on.
	interruptedID := uuid.NewString()
	deadline := time.Now().Add(time.Minute)
	intent, err := json.Marshal(map[string]any{"resume": resumeContext{
		Version: 1, ChatID: botID, UserID: "im-user-1", Query: "finish the task", DeadlineAt: &deadline,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO session_runs (
			run_id, bot_id, session_id, invocation_id, turn_id,
			turn_position, state, error_code, input_json, input_fingerprint
		) VALUES ($1, $2, $3, $4, $5, 1, 'lost', $6, $7, 'interrupted')
	`, interruptedID, botID, sessionID, "interrupted-"+interruptedID, uuid.NewString(),
		sessionruntime.RunErrorInterrupted, intent); err != nil {
		t.Fatalf("seed interrupted run: %v", err)
	}

	done, err := service.resumeInterruptedSession(ctx, dbsqlc.SessionRun{
		TeamID:    db.ParseUUIDOrEmpty(service.teamScopes.CurrentScope(ctx)),
		RunID:     db.ParseUUIDOrEmpty(interruptedID),
		BotID:     db.ParseUUIDOrEmpty(botID),
		SessionID: db.ParseUUIDOrEmpty(sessionID),
		InputJson: intent,
	})
	if err != nil {
		t.Fatalf("resumeInterruptedSession() error = %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("resume did not finish")
	}

	rows := historyRowsFor(t, fixture, sessionID)
	if len(rows) != 0 {
		t.Fatalf("resume wrote conversation history: %+v", rows)
	}
}
