package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/pending"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// resumeLedgerQueries keeps the harness's configuration fakes and sends the
// resume intent reads and writes to PostgreSQL.
type resumeLedgerQueries struct {
	noDecisionQueries
	db *dbsqlc.Queries
}

func (q resumeLedgerQueries) SaveSessionRunResumeContext(ctx context.Context, arg dbsqlc.SaveSessionRunResumeContextParams) (int64, error) {
	return q.db.SaveSessionRunResumeContext(ctx, arg)
}

func (q resumeLedgerQueries) ListInterruptedSessionRuns(ctx context.Context, after pgtype.UUID) ([]dbsqlc.SessionRun, error) {
	return q.db.ListInterruptedSessionRuns(ctx, after)
}

func (q resumeLedgerQueries) GetSessionInterruptedRun(ctx context.Context, sessionID pgtype.UUID) (dbsqlc.SessionRun, error) {
	return q.db.GetSessionInterruptedRun(ctx, sessionID)
}

func (q resumeLedgerQueries) RetireSupersededInterruptedSessionRuns(ctx context.Context) (int64, error) {
	return q.db.RetireSupersededInterruptedSessionRuns(ctx)
}

func (q resumeLedgerQueries) RetireInterruptedSessionRun(ctx context.Context, runID pgtype.UUID) (int64, error) {
	return q.db.RetireInterruptedSessionRun(ctx, runID)
}

// resumeProcess is one server process: its runtime and its application
// service, with resume enabled the way the server configures it.
type resumeProcess struct {
	manager *sessionruntime.Manager
	service *Service
}

func hasResumeIntent(t *testing.T, h wsStepHistoryHarness, runID string) bool {
	t.Helper()
	var raw []byte
	if err := h.pool.QueryRow(context.Background(), `SELECT input_json FROM session_runs WHERE run_id = $1`, runID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	_, ok := input["resume"]
	return ok
}

// sessionRun is one run of the session as the ledger holds it.
type sessionRun struct {
	invocation, state string
	position          int64
	created           time.Time
	hasResumeIntent   bool
}

// resumeTurnMessages serves the history of an interrupted turn and resolves
// the turn's request row the way the PostgreSQL store does.
type resumeTurnMessages struct {
	driveHistoryMessages
	requestMessageID string
}

func (m *resumeTurnMessages) GetRunTurnRequest(_ context.Context, _, _, rootTurnID string) (messagepkg.RunTurnRequest, error) {
	return messagepkg.RunTurnRequest{MessageID: m.requestMessageID, TurnID: rootTurnID}, nil
}

// startContinuation starts the continuation of row the way pending does once it
// has decided the session's turn has come.
func startContinuation(ctx context.Context, s *Service, row dbsqlc.SessionRun) (<-chan struct{}, error) {
	return s.continuer.Start(ctx, pending.Continuation{Run: row})
}

// resumeRequestMessages is a history that holds the request row of whichever
// run is resumed.
func resumeRequestMessages() *agenttest.RecordingMessages {
	return &agenttest.RecordingMessages{RunRequest: &messagepkg.RunTurnRequest{MessageID: uuid.NewString(), TurnID: uuid.NewString(), TurnPosition: 1}}
}
