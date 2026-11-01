package timeline

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/felinics/memoh/internal/chat/checkpoint"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/dbtest"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// replayTestSession opens the test database and creates a bot session to
// replay, removed with its bot when the test ends.
type replayTestSession struct {
	pool             *pgxpool.Pool
	queries          *postgresstore.Queries
	botID, sessionID string
	exec             func(sql string, args ...any)
}

func openReplayTestSession(t *testing.T) (context.Context, replayTestSession) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_DSN to run the timeline PostgreSQL integration")
	}
	if os.Getenv("TEST_POSTGRES_BOOTSTRAP_SCHEMA") == "1" {
		if err := dbtest.MigratePostgresUp(dsn); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	ctx := context.Background()
	pool, err := dbpkg.OpenPostgresDSN(ctx, dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	userID, botID, sessionID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec(`WITH u AS (INSERT INTO users (id, username, is_active) VALUES ($1::uuid, $1::text, true) RETURNING id)
		INSERT INTO team_members (user_id, role) SELECT id, 'admin' FROM u`, userID)
	exec(`INSERT INTO bots (id, owner_user_id, name) VALUES ($1::uuid, $2, $1::text)`, botID, userID)
	exec(`INSERT INTO bot_sessions (id, bot_id, channel_type) VALUES ($1, $2, 'local')`, sessionID, botID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM bots WHERE id = $1`, botID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})

	return ctx, replayTestSession{
		pool:      pool,
		queries:   postgresstore.NewQueriesWithPool(pool, dbsqlc.New(pool)),
		botID:     botID,
		sessionID: sessionID,
		exec:      exec,
	}
}

// Replay and classification read the session's checkpoint the same way: a
// message whose rows it folds leaves replay unless it changed after its
// rows were written, a message whose rows it leaves raw replays whole, and
// chatter without rows replays.
func TestPostgresReplayAndClassificationFollowTheCheckpoint(t *testing.T) {
	ctx, session := openReplayTestSession(t)
	queries, botID, sessionID, exec := session.queries, session.botID, session.sessionID, session.exec
	events := NewEventStore(nil, queries)
	received := time.Now().Add(-time.Hour).UnixMilli()
	persist := func(event CanonicalEvent) (string, int64) {
		t.Helper()
		id, stamped, err := events.PersistEvent(ctx, botID, sessionID, event)
		if err != nil || id == "" {
			t.Fatalf("persist %T: id %q, %v", event, id, err)
		}
		return id, eventCursorOf(stamped)
	}
	message := func(id string, atMs int64) MessageEvent {
		return MessageEvent{SessionID: sessionID, MessageID: id, ReceivedAtMs: atMs, Content: []ContentNode{{Type: "text", Text: id}}}
	}
	// row writes the context row an inbound message's event produced, at the
	// start of its own turn.
	rows := make(map[string]checkpoint.Row)
	row := func(messageID, eventID string, turn int64) {
		t.Helper()
		id := uuid.NewString()
		exec(`INSERT INTO bot_history_messages
			(id, bot_id, session_id, role, content, source_message_id, event_id, turn_id, turn_position, turn_message_seq, turn_visible, created_at)
			VALUES ($1, $2, $3, 'user', '{"role":"user","content":"m"}'::jsonb, $4, $5, $6, $7, 1, true, clock_timestamp())`,
			id, botID, sessionID, messageID, eventID, uuid.NewString(), turn)
		rows[messageID] = checkpoint.Row{ID: id, Position: messagepkg.HistoryPosition{Turn: turn, Seq: 1}}
	}

	cursors := make(map[string]int64)
	for i, id := range []string{"m1", "m2", "m3", "m4"} {
		eventID, cursor := persist(message(id, received+int64(i)*1000))
		cursors[id] = cursor
		if id != "m4" {
			row(id, eventID, int64(i+1))
		}
	}
	store := checkpoint.NewStore(nil, queries)
	summaryID := uuid.NewString()
	exec(`INSERT INTO bot_history_message_compacts (id, bot_id, session_id, status, summary, completed_at)
		VALUES ($1, $2, $3, 'ok', 'm1 and m2', now())`, summaryID, botID, sessionID)
	if err := queries.InTx(ctx, func(q dbstore.Queries) error {
		_, err := store.Commit(ctx, q, checkpoint.Fold{
			SessionID: sessionID, SummaryID: summaryID, Summary: "m1 and m2",
			Folded: []checkpoint.Row{rows["m1"], rows["m2"]},
			Read:   []string{rows["m1"].ID, rows["m2"].ID, rows["m3"].ID},
		})
		return err
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	persist(EditEvent{SessionID: sessionID, MessageID: "m2", ReceivedAtMs: time.Now().UnixMilli(), Content: []ContentNode{{Type: "text", Text: "m2 edited"}}})

	replayed := func() []string {
		t.Helper()
		loaded, err := events.LoadEventsForReplay(ctx, botID, sessionID)
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		var got []string
		for _, event := range loaded {
			got = append(got, string(event.Kind())+":"+extractExternalMessageID(event))
		}
		return got
	}
	if got, want := replayed(), []string{"message:m2", "message:m3", "message:m4", "edit:m2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("replay = %v, want %v", got, want)
	}

	source := NewCheckpointSource(nil, queries, messagepkg.NewService(nil, queries))
	rc := RenderedContext{{MessageID: "m1"}, {MessageID: "m2"}, {MessageID: "m3"}, {MessageID: "m4"}}
	held, err := source.Load(ctx, sessionID, rc)
	if err != nil {
		t.Fatalf("load checkpoint: %v", err)
	}
	want := map[string]CheckpointMessage{
		"m1": {EventCursor: cursors["m1"]},
		"m2": {EventCursor: cursors["m2"]},
		"m3": {Kept: true, EventCursor: cursors["m3"]},
	}
	if !reflect.DeepEqual(held.Messages, want) {
		t.Fatalf("classified = %+v, want %+v", held.Messages, want)
	}
}

// replayPageMeasuringEveryCandidate is the page as it reads when every
// candidate older than the cursor is measured before the page is cut: the
// definition the replay page query must keep while it measures only the page.
const replayPageMeasuringEveryCandidate = `
WITH candidates AS MATERIALIZED (
  SELECT event.id, event.received_at_ms, event.created_at,
    octet_length(event.event_data::text)::BIGINT AS payload_bytes
  FROM bot_session_events event
  WHERE event.team_id = public.memoh_current_team_id()
    AND event.session_id = $1
    AND (NOT $2::boolean OR (event.received_at_ms, event.created_at, event.id) < ($3::BIGINT, $4::TIMESTAMPTZ, $5::UUID))
    AND NOT (event.external_message_id IS NOT NULL AND event.external_message_id = ANY($6::text[]))
), ranked AS MATERIALIZED (
  SELECT candidates.*, SUM(payload_bytes) OVER (ORDER BY received_at_ms DESC, created_at DESC, id DESC)::BIGINT AS cumulative_bytes
  FROM candidates
)
SELECT id::text, payload_bytes, cumulative_bytes
FROM ranked
WHERE cumulative_bytes <= $7::BIGINT
ORDER BY received_at_ms DESC, created_at DESC, id DESC
LIMIT $8`

// The replay page is cut by the keyset before payloads are measured. A
// cumulative byte count includes only the events ahead of it, so every page
// of every replay admits the same events with the same counts as measuring
// all older candidates first, across ties on received time, folded messages,
// and budgets that end inside a page.
func TestPostgresReplayPageMeasuresOnlyThePage(t *testing.T) {
	ctx, session := openReplayTestSession(t)
	tied := time.Now().Add(-time.Hour)
	for i := range 60 {
		var externalID any
		if i%5 != 0 {
			externalID = fmt.Sprintf("m%02d", i)
		}
		// Every third event shares its received time and creation time with
		// the next, so the id decides their order.
		receivedAt, createdAt := int64(1000+i), tied.Add(time.Duration(i)*time.Millisecond)
		if i%3 == 1 {
			receivedAt, createdAt = int64(1000+i-1), tied.Add(time.Duration(i-1)*time.Millisecond)
		}
		session.exec(`INSERT INTO bot_session_events (bot_id, session_id, event_kind, event_data, external_message_id, received_at_ms, created_at)
			VALUES ($1, $2, 'message', jsonb_build_object('text', repeat('x', $3::int)), $4, $5, $6)`,
			session.botID, session.sessionID, 10+(i*37)%400, externalID, receivedAt, createdAt)
	}
	pgSessionID, err := dbpkg.ParseUUID(session.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	folded := []string{"m03", "m04", "m21", "m58"}

	type admitted struct {
		id                string
		payload, cumulate int64
	}
	for _, pageSize := range []int32{1, 7, 25, 256} {
		for _, budget := range []int64{150, 2_000, 9_000, 1 << 20} {
			t.Run(fmt.Sprintf("page %d budget %d", pageSize, budget), func(t *testing.T) {
				params := dbsqlc.ListSessionEventsBySessionPageBeforeWithinBytesParams{
					SessionID: pgSessionID, FoldedMessageIds: folded, MaxBytes: budget, PageSize: pageSize,
				}
				for pageNumber := 0; ; pageNumber++ {
					page, err := session.queries.ListSessionEventsBySessionPageBeforeWithinBytes(ctx, params)
					if err != nil {
						t.Fatal(err)
					}
					got := make([]admitted, 0, len(page))
					for _, row := range page {
						got = append(got, admitted{row.ID.String(), row.PayloadBytes, row.CumulativeBytes})
					}
					rows, err := session.pool.Query(ctx, replayPageMeasuringEveryCandidate, pgSessionID, params.HasCursor,
						params.BeforeReceivedAtMs, params.BeforeCreatedAt, params.BeforeID, folded, params.MaxBytes, params.PageSize)
					if err != nil {
						t.Fatal(err)
					}
					want := []admitted{}
					for rows.Next() {
						var a admitted
						if err := rows.Scan(&a.id, &a.payload, &a.cumulate); err != nil {
							t.Fatal(err)
						}
						want = append(want, a)
					}
					rows.Close()
					if err := rows.Err(); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("page %d = %v, want %v", pageNumber, got, want)
					}
					if len(page) == 0 {
						return
					}
					last := page[len(page)-1]
					params.HasCursor = true
					params.BeforeReceivedAtMs, params.BeforeCreatedAt, params.BeforeID = last.ReceivedAtMs, last.CreatedAt, last.ID
					params.MaxBytes -= last.CumulativeBytes
					if len(page) < int(pageSize) || params.MaxBytes <= 0 {
						return
					}
				}
			})
		}
	}
}
