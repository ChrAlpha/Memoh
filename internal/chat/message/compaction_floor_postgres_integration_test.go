package message

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

// floorFixture is six turns of two rows each, every row 500 bytes of content
// and created a second after the one before it.
type floorFixture struct {
	turns []HistoryTurn
	// users and answers name each turn's rows, oldest turn first.
	users, answers []Message
	at             func(row Message) time.Time
}

func setupFloorFixture(t *testing.T, ctx context.Context, tx pgx.Tx) floorFixture {
	t.Helper()
	svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))
	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	f := floorFixture{}
	created := map[string]time.Time{}
	for i := 0; i < 6; i++ {
		user, answer, turn := persistPostgresTurn(t, ctx, svc)
		f.users, f.answers, f.turns = append(f.users, user), append(f.answers, answer), append(f.turns, turn)
		for j, id := range []string{user.ID, answer.ID} {
			at := base.Add(time.Duration(2*i+j) * time.Second)
			created[id] = at
			if _, err := tx.Exec(ctx, `UPDATE bot_history_messages
				SET created_at = $2,
				    content = jsonb_build_object('role', role, 'content',
				      repeat('x', 500 - octet_length(jsonb_build_object('role', role, 'content', '')::text)))
				WHERE id = $1`, id, at); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.at = func(row Message) time.Time { return created[row.ID] }
	return f
}

func floorTurnFor(t *testing.T, ctx context.Context, tx pgx.Tx, params dbsqlc.GetCompactionFloorTurnParams) (int64, bool) {
	t.Helper()
	params.SessionID = mustTestUUID(t, postgresMessageTestSessionID)
	params.BytesPerToken = 4
	row, err := dbsqlc.New(tx).GetCompactionFloorTurn(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("GetCompactionFloorTurn: %v", err)
	}
	return row.FloorTurnPosition, true
}

// A floor keeps what the last request read, at least the history budget, and
// every turn after it, or the budget back from the newest turn when no request
// has been answered, counted over candidate content before the rows the
// request keeps. It leaves out every older turn, and never reaches a protected
// row, an inbound message the request keeps, or an edit's cutoff.
func TestPostgresCompactionFloorKeepsWhatTheLastRequestRead(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)
	f := setupFloorFixture(t, ctx, tx)
	position := func(i int) int64 { return f.turns[i].Position }

	// 2400 bytes are kept: the 1000-byte turns 6 to 4, as the 3000 after
	// turn 3 exceed it.
	if got, ok := floorTurnFor(t, ctx, tx, dbsqlc.GetCompactionFloorTurnParams{HistoryBudgetTokens: 600}); !ok || got != position(2) {
		t.Fatalf("floor = %d (%t), want turn 3 at %d", got, ok, position(2))
	}
	if _, ok := floorTurnFor(t, ctx, tx, dbsqlc.GetCompactionFloorTurnParams{HistoryBudgetTokens: 1500}); ok {
		t.Fatal("a backlog the budget holds got a floor")
	}
	// The protected input is the current turn's own, outside the backlog.
	current := dbsqlc.GetCompactionFloorTurnParams{HistoryBudgetTokens: 600, ProtectedMessageIds: []pgtype.UUID{mustTestUUID(t, f.users[5].ID)}}
	if got, ok := floorTurnFor(t, ctx, tx, current); !ok || got != position(1) {
		t.Fatalf("floor beside the current input = %d (%t), want turn 2", got, ok)
	}
	// An edit of turn 5 reads only what precedes it.
	edit := dbsqlc.GetCompactionFloorTurnParams{HistoryBudgetTokens: 600, CreatedBefore: pgtype.Timestamptz{Time: f.at(f.users[4]), Valid: true}}
	if got, ok := floorTurnFor(t, ctx, tx, edit); !ok || got != position(0) {
		t.Fatalf("floor before the cutoff = %d (%t), want turn 1", got, ok)
	}
	// An inbound message the request keeps, received with turn 4, holds the
	// floor below everything from then on.
	if _, err := dbsqlc.New(tx).CreateSessionEvent(ctx, dbsqlc.CreateSessionEventParams{
		BotID: mustTestUUID(t, postgresMessageTestBotID), SessionID: mustTestUUID(t, postgresMessageTestSessionID),
		EventKind: "message", EventData: []byte(`{}`), ExternalMessageID: pgtype.Text{String: "inbound-4", Valid: true},
		ReceivedAtMs: f.at(f.users[3]).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	inbound := dbsqlc.GetCompactionFloorTurnParams{HistoryBudgetTokens: 600, ProtectedExternalMessageIds: []string{"inbound-4"}}
	if _, ok := floorTurnFor(t, ctx, tx, inbound); ok {
		t.Fatal("the floor reached the turns an inbound message the request keeps arrived in")
	}
	// An answer of turn 2 committed after that message arrived holds the
	// floor below turn 2, though turn 3 alone exceeds what is kept.
	if _, err := tx.Exec(ctx, `UPDATE bot_history_messages SET created_at = $2 WHERE id = $1`, f.answers[1].ID, f.at(f.users[3]).Add(500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	inbound.HistoryBudgetTokens = 200
	if got, ok := floorTurnFor(t, ctx, tx, inbound); !ok || got != position(0) {
		t.Fatalf("floor below a late row = %d (%t), want turn 1", got, ok)
	}
	// The last request, answering turn 3, read 300 tokens: the 1200 bytes back
	// from turn 3 are kept, and the turns after it.
	if _, err := tx.Exec(ctx, `UPDATE bot_history_messages SET usage = '{"inputTokens": 300}' WHERE id = $1`, f.answers[2].ID); err != nil {
		t.Fatal(err)
	}
	if got, ok := floorTurnFor(t, ctx, tx, dbsqlc.GetCompactionFloorTurnParams{HistoryBudgetTokens: 200}); !ok || got != position(0) {
		t.Fatalf("floor under the request answering turn 3 = %d (%t), want turn 1", got, ok)
	}
	// A newer request, answering turn 5, read 500 tokens: 2000 bytes back from
	// turn 5.
	if _, err := tx.Exec(ctx, `UPDATE bot_history_messages SET usage = '{"inputTokens": 500}' WHERE id = $1`, f.answers[4].ID); err != nil {
		t.Fatal(err)
	}
	if got, ok := floorTurnFor(t, ctx, tx, dbsqlc.GetCompactionFloorTurnParams{HistoryBudgetTokens: 200}); !ok || got != position(1) {
		t.Fatalf("floor under the request answering turn 5 = %d (%t), want turn 2", got, ok)
	}
}

// A floor claims every candidate row of its turns, reclaiming a stale
// attempt's, and commits as a summary of its own: the history then starts
// after it, replay leaves out the events under it, and only a rewind to
// before it retires it.
func TestPostgresCompactionFloorClaimsItsTurns(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)
	f := setupFloorFixture(t, ctx, tx)
	q := dbsqlc.New(tx)
	sessionID, botID := mustTestUUID(t, postgresMessageTestSessionID), mustTestUUID(t, postgresMessageTestBotID)

	var stale string
	if err := tx.QueryRow(ctx, `INSERT INTO bot_history_message_compacts (bot_id, session_id, status, started_at)
		VALUES ($1, $2, 'pending', now() - INTERVAL '1 hour') RETURNING id::text`, postgresMessageTestBotID, postgresMessageTestSessionID).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE bot_history_messages SET compact_id = $1 WHERE id = $2`, stale, f.users[0].ID); err != nil {
		t.Fatal(err)
	}
	attempt, err := q.CreateCompactionLog(ctx, dbsqlc.CreateCompactionLogParams{BotID: botID, SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	omitted, err := q.MarkMessagesOmitted(ctx, dbsqlc.MarkMessagesOmittedParams{CompactID: attempt.ID, TurnPosition: f.turns[2].Position})
	if err != nil {
		t.Fatal(err)
	}
	if omitted.OmittedCount != 6 || !omitted.FirstCreatedAt.Time.Equal(f.at(f.users[0])) || !omitted.LastCreatedAt.Time.Equal(f.at(f.answers[2])) {
		t.Fatalf("omitted = %+v, want the six rows of turns 1 to 3", omitted)
	}
	var staleStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM bot_history_message_compacts WHERE id = $1`, stale).Scan(&staleStatus); err != nil || staleStatus != "error" {
		t.Fatalf("stale attempt = %q (%v), want it reclaimed", staleStatus, err)
	}
	floorMs := omitted.LastCreatedAt.Time.UnixMilli()
	if _, err := q.CompleteCompactionLog(ctx, dbsqlc.CompleteCompactionLogParams{
		ID: attempt.ID, Status: "ok", Summary: "6 earlier messages are left out", MessageCount: 6, Coverage: []byte("[]"),
		AnchorStartMs: omitted.FirstCreatedAt.Time.UnixMilli(), AnchorEndMs: floorMs, FloorMs: floorMs,
		FloorNotice: "6 earlier messages are left out",
	}); err != nil {
		t.Fatalf("commit the floor: %v", err)
	}
	assertHistoryAnchor := func(want time.Time) {
		t.Helper()
		anchor, err := q.GetSessionHistoryAnchor(ctx, dbsqlc.GetSessionHistoryAnchorParams{SessionID: sessionID})
		if err != nil || !anchor.Time.Equal(want) {
			t.Fatalf("anchor = %v (%v), want %v", anchor.Time, err, want)
		}
	}
	assertHistoryAnchor(f.at(f.users[3]))
	if _, ok := floorTurnFor(t, ctx, tx, dbsqlc.GetCompactionFloorTurnParams{HistoryBudgetTokens: 600}); ok {
		t.Fatal("the turns above the floor got another floor")
	}

	for i, receivedAt := range []int64{floorMs, floorMs + 1} {
		if _, err := q.CreateSessionEvent(ctx, dbsqlc.CreateSessionEventParams{
			BotID: botID, SessionID: sessionID, EventKind: "message", EventData: []byte(`{}`),
			ExternalMessageID: pgtype.Text{String: []string{"under", "above"}[i], Valid: true}, ReceivedAtMs: receivedAt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	replayed, err := q.ListSessionEventsBySessionPageBeforeWithinBytes(ctx, dbsqlc.ListSessionEventsBySessionPageBeforeWithinBytesParams{
		SessionID: sessionID, FloorMs: floorMs, CoveredExternalMessages: []byte(`{}`), CoveredEventCursors: []byte(`{}`),
		MaxBytes: 1 << 20, PageSize: 10,
	})
	if err != nil || len(replayed) != 1 || replayed[0].ExternalMessageID.String != "above" {
		t.Fatalf("replayed = %+v (%v), want only the event above the floor", replayed, err)
	}

	// A rewind to turn 5 keeps the floor below it; hiding turn 2 retires it.
	if _, err := tx.Exec(ctx, `SELECT public.memoh_rewind_compaction($1, $2)`, postgresMessageTestSessionID, f.at(f.users[4])); err != nil {
		t.Fatal(err)
	}
	assertSessionCompactionEpoch(t, ctx, tx, 1)
	assertHistoryAnchor(f.at(f.users[3]))
	if err := q.HideMessagesByHistoryTurn(ctx, mustTestUUID(t, f.turns[1].ID)); err != nil {
		t.Fatal(err)
	}
	assertHistoryAnchor(f.at(f.users[0]))
}
