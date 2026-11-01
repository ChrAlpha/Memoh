package timeline

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/chat/checkpoint"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/dbtest"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// classifyEveryMessage asks how a checkpoint stands for each named inbound
// message from all of its context rows at once: kept when one of them is
// after the position or kept by reference, folded otherwise.
const classifyEveryMessage = `
SELECT m.source_message_id::text,
  bool_or((m.turn_position, m.turn_message_seq) > ($1::bigint, $2::bigint) OR m.id = ANY($3::uuid[]))::boolean,
  COALESCE(MAX((event.event_data->>'event_cursor')::bigint), 0)::bigint
FROM bot_visible_history_messages m
LEFT JOIN bot_session_events event
  ON event.team_id = public.memoh_current_team_id()
 AND event.id = m.event_id
WHERE m.team_id = public.memoh_current_team_id()
  AND m.session_id = $4
  AND m.source_message_id = ANY($5::text[])
  AND (m.metadata->>'trigger_mode' IS NULL OR m.metadata->>'trigger_mode' != 'passive_sync')
GROUP BY m.source_message_id`

// A source that composes a session again and again folds exactly the
// messages a classification of every message from all of its rows folds,
// however the history moves between loads: new messages and rows, rows for
// messages already folded, rows after the position hidden, folds onto the
// checkpoint, and rewinds that hide rows it accounts for.
func TestPostgresCheckpointSourceFoldsWhatEveryRowClassifies(t *testing.T) {
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

	queries := postgresstore.NewQueriesWithPool(pool, dbsqlc.New(pool))
	events := NewEventStore(nil, queries)
	store := checkpoint.NewStore(slog.New(slog.DiscardHandler), queries)
	rows := messagepkg.NewService(nil, queries)
	source := NewCheckpointSource(nil, queries, rows)
	random := rand.New(rand.NewPCG(1700, 1721)) //nolint:gosec // deterministic test schedule

	var (
		messages []string
		turn     int64
		received = time.Now().Add(-time.Hour).UnixMilli()
	)
	persist := func(event CanonicalEvent) string {
		t.Helper()
		id, _, err := events.PersistEvent(ctx, botID, sessionID, event)
		if err != nil || id == "" {
			t.Fatalf("persist %T: id %q, %v", event, id, err)
		}
		return id
	}
	// row writes a context row for an inbound message in a turn of its own
	// or, with appended, after the newest row of the newest turn.
	row := func(messageID string, eventID any, metadata string, appended bool) {
		t.Helper()
		if !appended || turn == 0 {
			turn++
		}
		exec(`INSERT INTO bot_history_messages
			(bot_id, session_id, role, content, metadata, source_message_id, event_id, turn_id, turn_position, turn_message_seq, turn_visible, created_at)
			SELECT $1, $2, 'user', '{"role":"user","content":"m"}'::jsonb, $3::jsonb, $4, $5, gen_random_uuid(), $6,
			  COALESCE(MAX(turn_message_seq), 0) + 1, true, clock_timestamp()
			FROM bot_history_messages WHERE session_id = $2 AND turn_position = $6`,
			botID, sessionID, metadata, messageID, eventID, turn)
	}
	inbound := func() {
		t.Helper()
		id := fmt.Sprintf("m%d", len(messages)+1)
		received += 1000
		eventID := persist(MessageEvent{SessionID: sessionID, MessageID: id, ReceivedAtMs: received, Content: []ContentNode{{Type: "text", Text: id}}})
		messages = append(messages, id)
		switch random.IntN(10) {
		case 0: // chatter without rows
		case 1:
			row(id, nil, `{}`, false) // a row whose event is unknown
		case 2:
			row(id, eventID, `{"trigger_mode":"passive_sync"}`, false)
		default:
			row(id, eventID, `{}`, random.IntN(4) == 0)
		}
	}
	// fold commits a summary onto the active checkpoint of some of the
	// oldest context rows after it, keeping the rest of what it read.
	fold := func() {
		t.Helper()
		parent, err := store.Active(ctx, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		candidates, err := pool.Query(ctx, `SELECT id::text, turn_position, turn_message_seq FROM bot_visible_history_messages
			WHERE session_id = $1 AND (turn_position, turn_message_seq) > ($2, $3)
			  AND (metadata->>'trigger_mode' IS NULL OR metadata->>'trigger_mode' != 'passive_sync')
			ORDER BY turn_position, turn_message_seq LIMIT $4`, sessionID, parent.Position.Turn, parent.Position.Seq, 1+random.IntN(12))
		if err != nil {
			t.Fatal(err)
		}
		var read []checkpoint.Row
		for candidates.Next() {
			var row checkpoint.Row
			if err := candidates.Scan(&row.ID, &row.Position.Turn, &row.Position.Seq); err != nil {
				t.Fatal(err)
			}
			read = append(read, row)
		}
		candidates.Close()
		if len(read) == 0 {
			return
		}
		f := checkpoint.Fold{SessionID: sessionID, Parent: parent, SummaryID: uuid.NewString(), Summary: "folded"}
		for _, row := range read {
			f.Read = append(f.Read, row.ID)
			if random.IntN(4) != 0 {
				f.Folded = append(f.Folded, row)
			}
		}
		exec(`INSERT INTO bot_history_message_compacts (id, bot_id, session_id, status, summary, completed_at)
			VALUES ($1, $2, $3, 'ok', 'folded', now())`, f.SummaryID, botID, sessionID)
		if err := queries.InTx(ctx, func(q dbstore.Queries) error {
			_, err := store.Commit(ctx, q, f)
			return err
		}); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	// hide makes a visible context row invisible, rewinding the session as
	// every writer that hides rows does.
	hide := func(behind bool) {
		t.Helper()
		active, err := store.Active(ctx, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		comparison := ">"
		if behind {
			comparison = "<="
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SELECT 1 FROM bot_sessions WHERE id = $1 FOR UPDATE`, sessionID); err != nil {
			t.Fatal(err)
		}
		var position, seq pgtype.Int8
		if err := tx.QueryRow(ctx, `WITH hidden AS (
				UPDATE bot_history_messages SET turn_visible = false
				WHERE id = (SELECT id FROM bot_visible_history_messages
					WHERE session_id = $1 AND (turn_position, turn_message_seq) `+comparison+` ($2, $3)
					ORDER BY random() LIMIT 1)
				RETURNING turn_position, turn_message_seq)
			SELECT turn_position, turn_message_seq FROM hidden`, sessionID, active.Position.Turn, active.Position.Seq).Scan(&position, &seq); err != nil {
			return // nothing to hide there
		}
		if _, err := tx.Exec(ctx, `SELECT public.memoh_rewind_history_checkpoint($1, $2, $3)`, sessionID, position.Int64, seq.Int64); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// classified is what a classification of every message from all of
	// its rows folds.
	classified := func(rc RenderedContext) map[string]FoldedMessage {
		t.Helper()
		active, err := store.Active(ctx, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if active.ID == "" {
			return nil
		}
		result, err := pool.Query(ctx, classifyEveryMessage, active.Position.Turn, active.Position.Seq, active.RowUUIDs(), sessionID, inboundMessageIDs(rc))
		if err != nil {
			t.Fatal(err)
		}
		defer result.Close()
		folded := make(map[string]FoldedMessage)
		for result.Next() {
			var (
				id     string
				kept   bool
				cursor int64
			)
			if err := result.Scan(&id, &kept, &cursor); err != nil {
				t.Fatal(err)
			}
			if !kept {
				folded[id] = FoldedMessage{EventCursor: cursor}
			}
		}
		return folded
	}

	edited := make(map[string]bool)
	var compared, foldedSeen int
	for step := 0; step < 400; step++ {
		switch n := random.IntN(20); {
		case n < 10:
			inbound()
		case n < 12 && len(messages) > 0:
			// Another row for a message, folded or not, written from its
			// edit the first time and from an unknown event after that.
			id := messages[random.IntN(len(messages))]
			var eventID any
			if !edited[id] {
				edited[id] = true
				received += 1000
				eventID = persist(EditEvent{SessionID: sessionID, MessageID: id, ReceivedAtMs: received, Content: []ContentNode{{Type: "text", Text: id + " edited"}}})
			}
			row(id, eventID, `{}`, random.IntN(2) == 0)
		case n < 14:
			hide(false)
		case n < 15:
			hide(true)
		default:
			fold()
		}
		rc := make(RenderedContext, 0, len(messages)+1)
		for _, id := range messages {
			rc = append(rc, RenderedSegment{MessageID: id})
		}
		rc = append(rc, RenderedSegment{MessageID: "never-seen"})
		want := classified(rc)
		held, err := source.Load(ctx, sessionID, rc)
		if err != nil {
			t.Fatalf("step %d: load: %v", step, err)
		}
		if len(held.Folded) == 0 && len(want) == 0 {
			held.Folded, want = nil, nil
		}
		if !reflect.DeepEqual(held.Folded, want) {
			t.Fatalf("step %d: folded %v, a classification of every row folds %v", step, held.Folded, want)
		}
		compared++
		foldedSeen += len(want)
	}
	if foldedSeen == 0 || compared != 400 {
		t.Fatalf("compared %d loads with %d folded messages in all, want folds to happen", compared, foldedSeen)
	}
}
