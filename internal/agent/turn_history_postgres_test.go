package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/felinics/memoh/internal/agent/admit"
	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/dbtest"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/runtimefence"
)

// newDriveHistoryFixture seeds one bot and session and wires the application's
// real stores over the test database.
func newDriveHistoryFixture(t *testing.T, mode directLifecycleModelMode) (driveHistoryFixture, *pgxpool.Pool, string, string) {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.OpenPostgres(t, ctx)
	botID, sessionID := agenttest.CreateSessionFixture(t, ctx, pool)
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

	store := messagepkg.NewService(slog.New(slog.DiscardHandler), queries)
	fixture := newChatTurnFixture(t, mode,
		withFixtureRuntime(manager),
		withFixtureMessages(store),
		// The real store, with the two decision reads a finishing run makes
		// answered as "none": this fixture creates no decisions.
		withFixtureQueries(func(dbstore.Queries) dbstore.Queries { return postgresNoDecisionQueries{Queries: queries} }))
	return driveHistoryFixture{chatTurnFixture: fixture, store: store}, pool, botID, sessionID
}

// driveHistoryFixture is a chat fixture over the real stores, with the message
// store the history is read back from.
type driveHistoryFixture struct {
	chatTurnFixture
	store *messagepkg.DBService
}

// postgresNoDecisionQueries is the real PostgreSQL store with the run's
// pending-decision reads answered as none.
type postgresNoDecisionQueries struct {
	*postgresstore.Queries
	noDecisionReads
}

// historyRowsFor lists the conversation's rows newest first, as the history
// endpoint would.
func historyRowsFor(t *testing.T, fixture driveHistoryFixture, sessionID string) []messagepkg.Message {
	t.Helper()
	rows, err := fixture.store.ListBySession(context.Background(), sessionID)
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
	drainTurn(t, handle)

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
	interruptedTurnID := uuid.NewString()
	deadline := time.Now().Add(time.Minute)
	intent, err := json.Marshal(map[string]any{"resume": admit.ResumeContext{
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
	`, interruptedID, botID, sessionID, "interrupted-"+interruptedID, interruptedTurnID,
		sessionruntime.RunErrorInterrupted, intent); err != nil {
		t.Fatalf("seed interrupted run: %v", err)
	}
	// The request the interrupted run was answering is already in history.
	requestID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO bot_history_messages (id, bot_id, session_id, role, content, run_id, turn_id, turn_position, turn_message_seq, turn_visible)
		VALUES ($1, $2, $3, 'user', '{"role":"user","content":"finish the task"}', $4, $5, 1, 1, true)
	`, requestID, botID, sessionID, interruptedID, interruptedTurnID); err != nil {
		t.Fatalf("seed request row: %v", err)
	}

	done, err := startContinuation(ctx, service, dbsqlc.SessionRun{
		TeamID:       db.ParseUUIDOrEmpty(runreq.SingleTeam("").CurrentScope(ctx)),
		RunID:        db.ParseUUIDOrEmpty(interruptedID),
		BotID:        db.ParseUUIDOrEmpty(botID),
		SessionID:    db.ParseUUIDOrEmpty(sessionID),
		TurnID:       db.ParseUUIDOrEmpty(interruptedTurnID),
		TurnPosition: 1,
		InputJson:    intent,
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
	if len(rows) != 1 || rows[0].ID != requestID {
		t.Fatalf("resume wrote conversation history beyond the request row: %+v", rows)
	}
}
