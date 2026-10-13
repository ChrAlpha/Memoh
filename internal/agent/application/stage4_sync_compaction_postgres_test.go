package application

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// stage4FixedSession creates the bot and session the drive fixture's turn
// runs in (the lifecycle test ids), so the real compaction service and the
// run read and write the same rows.
func stage4FixedSession(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	userID := uuid.New()
	name := "stage4-sync-" + uuid.NewString()
	cleanup := context.WithoutCancel(ctx)
	_, _ = pool.Exec(cleanup, "DELETE FROM bots WHERE id = $1", lifecycleTestBotID)
	if _, err := pool.Exec(ctx, `
		WITH created_user AS (
			INSERT INTO users (id, username, is_active) VALUES ($1, $2, true) RETURNING id
		)
		INSERT INTO team_members (user_id, role) SELECT id, 'admin' FROM created_user
	`, userID, name); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO bots (id, owner_user_id, name) VALUES ($1, $2, $3)`, lifecycleTestBotID, userID, name); err != nil {
		t.Fatalf("create bot: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO bot_sessions (id, bot_id, channel_type, runtime_type) VALUES ($1, $2, 'telegram', 'model')`, lifecycleTestSessionID, lifecycleTestBotID); err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(cleanup, "DELETE FROM bots WHERE id = $1", lifecycleTestBotID)
		_, _ = pool.Exec(cleanup, "DELETE FROM users WHERE id = $1", userID)
	})
}

// stage4Queries answers the compaction artifact reads from the database; the
// fixture's own fake answers them with an empty frontier.
type stage4Queries struct {
	noDecisionQueries
	real dbstore.Queries
}

func (q stage4Queries) ListCompactionArtifactLineageBySession(ctx context.Context, id pgtype.UUID) ([]sqlc.BotHistoryMessageCompact, error) {
	return q.real.ListCompactionArtifactLineageBySession(ctx, id)
}

func (q stage4Queries) GetCompactionLogByID(ctx context.Context, id pgtype.UUID) (sqlc.BotHistoryMessageCompact, error) {
	return q.real.GetCompactionLogByID(ctx, id)
}

func (q stage4Queries) ListCompactionArtifactParentIDsBySuccessor(ctx context.Context, arg sqlc.ListCompactionArtifactParentIDsBySuccessorParams) ([]pgtype.UUID, error) {
	return q.real.ListCompactionArtifactParentIDsBySuccessor(ctx, arg)
}

func (q stage4Queries) ListMessageRefsByCompactID(ctx context.Context, id pgtype.UUID) ([]sqlc.ListMessageRefsByCompactIDRow, error) {
	return q.real.ListMessageRefsByCompactID(ctx, id)
}

// stage4DriveSession is a drive-request fixture whose history is real
// PostgreSQL rows, whose compaction is the real compaction service, and whose
// model window is small enough that ten rows pass the blocking share of it.
func stage4DriveSession(t *testing.T) (*stage4Session, directLifecycleFixture, *driveRequestRecorder) {
	t.Helper()
	ctx := context.Background()
	pool := openTurnAdmissionPostgres(t, ctx)
	stage4FixedSession(t, ctx, pool)
	s := newStage4SessionFor(t, ctx, pool, lifecycleTestBotID, lifecycleTestSessionID)

	fixture, recorder := newDriveRequestFixture(t)
	withDriveRequestRuntime(t, fixture)
	queries := fixture.service.queries.(noDecisionQueries).directLifecycleQueries
	// Everything the fixture does not fake, the compaction frontier included,
	// is answered by the database.
	queries.Queries = s.service.queries
	for key, model := range queries.models {
		model.Config = []byte(`{"context_window":60000,"compatibilities":["tool-call"]}`)
		queries.models[key] = model
	}
	fixture.service.messageService = &pgContextMessages{db: s.messages, sessionID: s.sessionID}
	fixture.service.queries = stage4Queries{noDecisionQueries: fixture.service.queries.(noDecisionQueries), real: s.service.queries}
	fixture.service.compactionService = withoutModelRecord{s.compaction}
	fixture.service.compactionHTTPClient = &http.Client{Transport: s.summarizer}
	return s, fixture, recorder
}

// withoutModelRecord drops the summarizer's model record id: the fixture's
// model exists only in memory, and the artifact row's foreign key to the
// models table would refuse it.
type withoutModelRecord struct{ inner *compaction.Service }

func (w withoutModelRecord) RunCompactionSync(ctx context.Context, cfg compaction.TriggerConfig) (compaction.Result, error) {
	cfg.ModelRecordID = ""
	return w.inner.RunCompactionSync(ctx, cfg)
}

func startStage4Turn(t *testing.T, fixture directLifecycleFixture, query string) {
	t.Helper()
	handle, err := fixture.service.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion: 1, TeamID: "drive-request-team", Mode: turn.ModeChat,
		BotID: lifecycleTestBotID, ChatID: lifecycleTestBotID, ThreadID: lifecycleTestSessionID,
		Query: query, UserVisibleText: query,
	})
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	drainDriveHandle(t, handle)
}

// Context at the blocking share of the window is compacted once, before the
// provider request, and the request carries the summary in place of the rows
// it covers (ruling 1, the pre-turn synchronous compaction, K2). The history
// is real PostgreSQL rows, the compaction is the real service, and only the
// summarizer is a stand-in.
func TestStage4PreTurnCompactionReplacesCoveredRowsInTheProviderRequest(t *testing.T) {
	s, fixture, recorder := stage4DriveSession(t)
	s.padding = 3000
	s.persistConversation("a", 8)

	startStage4Turn(t, fixture, "what now?")

	if got := s.summarizer.callCount(); got != 1 {
		t.Fatalf("summarizer calls = %d, want exactly one synchronous compaction", got)
	}
	covered, raw := requireStage4Split(t, s)
	requests := requireDriveRequests(t, recorder)
	body := string(marshalDriveRequests(t, requests[:1]))
	for _, label := range covered {
		if strings.Contains(body, label+" padding") {
			t.Fatalf("the provider request still carries covered row %s", label)
		}
	}
	last := strings.Index(body, "SUMMARY-1")
	if last < 0 {
		t.Fatalf("the provider request has no summary:\n%s", body)
	}
	for _, label := range raw {
		at := strings.Index(body, label+" padding")
		if at < 0 {
			t.Fatalf("the provider request lost uncovered row %s", label)
		}
		if at < last {
			t.Fatalf("uncovered row %s comes before the summary", label)
		}
		last = at
	}
	if !strings.Contains(body, "what now?") {
		t.Fatal("the provider request lacks the current message")
	}
}

// A summarizer that fails does not stop the turn: the request still reaches
// the provider, no row is left claimed by a failed summary, and the context
// the turn goes on with is the history as it was (ruling 4, what does not fit
// goes to the provider as it is). The last check calls the backstop itself,
// because the request is also shaped by the context view's own budget.
func TestStage4PreTurnCompactionFailureSendsTheContextAsIs(t *testing.T) {
	s, fixture, recorder := stage4DriveSession(t)
	s.padding = 3000
	ids := s.persistConversation("a", 8)
	s.summarizer.refuseWith(http.StatusBadRequest)

	startStage4Turn(t, fixture, "what now?")

	if s.summarizer.callCount() == 0 {
		t.Fatal("the compaction never reached the summarizer; the test would pass without one")
	}
	body := string(marshalDriveRequests(t, requireDriveRequests(t, recorder)[:1]))
	if strings.Contains(body, "SUMMARY-") {
		t.Fatalf("the request carries a summary the summarizer never produced")
	}
	if !strings.Contains(body, "what now?") || !strings.Contains(body, "a-07 padding") {
		t.Fatal("the request lacks the current message or the newest row")
	}
	if covered, raw := s.compactedLabels(); len(covered) != 0 || len(raw) != len(ids) {
		t.Fatalf("a failed compaction left covered=%v raw=%v", covered, raw)
	}

	req := ChatRequest{BotID: s.botID, ChatID: s.botID, ThreadID: s.sessionID, RunID: "stage4-run"}
	assembled := s.assemble(ChatRequest{})
	before := stage4Labels(assembled.Messages)
	after, err := fixture.service.compactBeforeTurn(s.ctx, req, "", 60000, assembled)
	if err != nil {
		t.Fatalf("compactBeforeTurn: %v", err)
	}
	if got := stage4Labels(after.Messages); !slices.Equal(got, before) || len(before) != len(ids) {
		t.Fatalf("context after the failed backstop = %v, want the %d rows as they were (%v)", got, len(ids), before)
	}
}
