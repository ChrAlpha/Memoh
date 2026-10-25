package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/dbtest"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// historyFixture is a bot whose sessions hold turn-bound history rows
// written directly, so a test places each row exactly where it needs it.
type historyFixture struct {
	t       *testing.T
	ctx     context.Context
	pool    *pgxpool.Pool
	queries *postgresstore.Queries
	store   *Store
	botID   string
	session string
	turns   map[string]string
	rows    map[string]messagepkg.HistoryPosition
}

func newHistoryFixture(t *testing.T) *historyFixture {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_DSN to run the history checkpoint PostgreSQL integration")
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
	queries := postgresstore.NewQueriesWithPool(pool, dbsqlc.New(pool))
	f := &historyFixture{
		t:       t,
		ctx:     ctx,
		pool:    pool,
		queries: queries,
		store:   NewStore(nil, queries),
		botID:   uuid.NewString(),
		turns:   make(map[string]string),
		rows:    make(map[string]messagepkg.HistoryPosition),
	}
	userID := uuid.NewString()
	f.exec(`WITH u AS (INSERT INTO users (id, username, is_active) VALUES ($1::uuid, $1::text, true) RETURNING id)
		INSERT INTO team_members (user_id, role) SELECT id, 'admin' FROM u`, userID)
	f.exec(`INSERT INTO bots (id, owner_user_id, name) VALUES ($1::uuid, $2, $1::text)`, f.botID, userID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM bots WHERE id = $1`, f.botID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})
	f.session = f.newSession()
	return f
}

func (f *historyFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f *historyFixture) newSession() string {
	f.t.Helper()
	id := uuid.NewString()
	f.exec(`INSERT INTO bot_sessions (id, bot_id, channel_type) VALUES ($1, $2, 'local')`, id, f.botID)
	return id
}

// row writes a visible context row of the session at a turn position and a
// sequence in it.
func (f *historyFixture) row(session string, turn, seq int64) string {
	f.t.Helper()
	key := fmt.Sprintf("%s/%d", session, turn)
	turnID, ok := f.turns[key]
	if !ok {
		turnID = uuid.NewString()
		f.turns[key] = turnID
	}
	role := "user"
	if seq > 1 {
		role = "assistant"
	}
	id := uuid.NewString()
	f.exec(`INSERT INTO bot_history_messages
		(id, bot_id, session_id, role, content, turn_id, turn_position, turn_message_seq, turn_visible, created_at)
		VALUES ($1, $2, $3, $4, jsonb_build_object('role', $4::text, 'content', $5::text), $6, $7, $8, true, clock_timestamp())`,
		id, f.botID, session, role, fmt.Sprintf("turn %d message %d", turn, seq), turnID, turn, seq)
	f.rows[id] = messagepkg.HistoryPosition{Turn: turn, Seq: seq}
	return id
}

// summary writes a usable compaction log of the session.
func (f *historyFixture) summary(session, text string) string {
	f.t.Helper()
	id := uuid.NewString()
	f.exec(`INSERT INTO bot_history_message_compacts (id, bot_id, session_id, status, summary, completed_at)
		VALUES ($1, $2, $3, 'ok', $4, now())`, id, f.botID, session, text)
	return id
}

// fold commits a summary of the folded rows onto parent for the fixture's
// session, as a compaction pass that read every row of it does.
func (f *historyFixture) fold(parent Checkpoint, text string, folded ...string) (Checkpoint, error) {
	f.t.Helper()
	return f.foldRead(parent, text, f.sessionRows(), folded...)
}

func (f *historyFixture) foldRead(parent Checkpoint, text string, read []string, folded ...string) (Checkpoint, error) {
	f.t.Helper()
	rows := make([]Row, 0, len(folded))
	for _, id := range folded {
		rows = append(rows, Row{ID: id, Position: f.rows[id]})
	}
	fold := Fold{BotID: f.botID, SessionID: f.session, Parent: parent, SummaryID: f.summary(f.session, text), Summary: text, Folded: rows, Read: read}
	var created Checkpoint
	err := f.queries.InTx(f.ctx, func(q dbstore.Queries) error {
		var err error
		created, err = f.store.Commit(f.ctx, q, fold)
		return err
	})
	return created, err
}

func (f *historyFixture) mustFold(parent Checkpoint, text string, folded ...string) Checkpoint {
	f.t.Helper()
	created, err := f.fold(parent, text, folded...)
	if err != nil {
		f.t.Fatalf("fold %q: %v", text, err)
	}
	return created
}

func (f *historyFixture) sessionRows() []string {
	ids := make([]string, 0, len(f.rows))
	for id := range f.rows {
		ids = append(ids, id)
	}
	return ids
}

// pointer is the session's active checkpoint as stored: "" when it has none.
func (f *historyFixture) pointer(session string) string {
	f.t.Helper()
	var id pgtype.UUID
	if err := f.pool.QueryRow(f.ctx, `SELECT history_checkpoint_id FROM bot_sessions WHERE id = $1`, session).Scan(&id); err != nil {
		f.t.Fatalf("read active checkpoint: %v", err)
	}
	return id.String()
}

// appeared counts the session's rows on record as appearing below a
// checkpoint.
func (f *historyFixture) appeared(session string) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM bot_history_appeared_rows WHERE session_id = $1`, session).Scan(&n); err != nil {
		f.t.Fatalf("count appeared rows: %v", err)
	}
	return n
}

func (f *historyFixture) active() Checkpoint {
	f.t.Helper()
	active, err := f.store.Active(f.ctx, f.session)
	if err != nil {
		f.t.Fatalf("active checkpoint: %v", err)
	}
	return active
}

func items(entries ...Item) []Item { return entries }

func sameItems(got, want []Item) bool {
	return slices.EqualFunc(got, want, func(a, b Item) bool {
		return a.RowID == b.RowID && a.SummaryID == b.SummaryID && (b.Summary == "" || a.Summary == b.Summary)
	})
}

// A fold accounts for the history through the last row it folds: the rows
// there it leaves raw stay in place by reference, its summary follows them,
// and the next fold absorbs both into its own summary.
func TestPostgresCommitFoldsOntoTheActiveCheckpoint(t *testing.T) {
	f := newHistoryFixture(t)
	u1, a1 := f.row(f.session, 1, 1), f.row(f.session, 1, 2)
	u2, a2 := f.row(f.session, 2, 1), f.row(f.session, 2, 2)
	u3, _ := f.row(f.session, 3, 1), f.row(f.session, 3, 2)

	first := f.mustFold(Checkpoint{}, "first", u1, a1, a2)
	if first.ParentID != "" || first.Position != (messagepkg.HistoryPosition{Turn: 2, Seq: 2}) {
		t.Fatalf("first = parent %q at %+v, want no parent at 2.2", first.ParentID, first.Position)
	}
	if want := items(Item{RowID: u2}, Item{SummaryID: first.SummaryIDs()[0], Summary: "first"}); !sameItems(first.Items, want) {
		t.Fatalf("first items = %+v, want %+v", first.Items, want)
	}
	if got := f.pointer(f.session); got != first.ID {
		t.Fatalf("active = %q, want %q", got, first.ID)
	}
	read := f.active()
	if read.ID != first.ID || !sameItems(read.Items, first.Items) || read.Items[1].Summary != "first" {
		t.Fatalf("Active = %+v, want %+v with the summary text", read, first)
	}

	second := f.mustFold(read, "second", u2, u3)
	if second.ParentID != first.ID || second.Position != (messagepkg.HistoryPosition{Turn: 3, Seq: 1}) {
		t.Fatalf("second = parent %q at %+v, want %q at 3.1", second.ParentID, second.Position, first.ID)
	}
	if len(second.Items) != 1 || second.Items[0].SummaryID == "" {
		t.Fatalf("second items = %+v, want only its summary", second.Items)
	}
	if got := f.active(); got.ID != second.ID || got.Items[0].Summary != "second" {
		t.Fatalf("Active = %+v, want %q with its text", got, second.ID)
	}
}

// A fold keeps the rows of its span it does not fold: one its pass read, an
// anchor, before its summary, where the request that read it places the
// summary, and one its pass never read after it, since the summary was
// written without it.
func TestPostgresCommitKeepsTheRowsItDoesNotFoldAroundItsSummary(t *testing.T) {
	f := newHistoryFixture(t)
	u1, a1 := f.row(f.session, 1, 1), f.row(f.session, 1, 2)
	read := f.sessionRows()
	u2, a2 := f.row(f.session, 2, 1), f.row(f.session, 2, 2)

	created, err := f.foldRead(Checkpoint{}, "summary", append(read, a2), a1, a2)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if want := items(Item{RowID: u1}, Item{SummaryID: created.SummaryIDs()[0]}, Item{RowID: u2}); !sameItems(created.Items, want) {
		t.Fatalf("items = %+v, want the anchor before the summary and the unread row after it", created.Items)
	}
}

// A fold commits only onto the history it read: not when another fold moved
// the session on, not when a row it folds stopped being context, and not
// when its parent no longer holds. Each refusal leaves the session where it
// was.
func TestPostgresCommitRefusesHistoryThatMoved(t *testing.T) {
	f := newHistoryFixture(t)
	u1, a1 := f.row(f.session, 1, 1), f.row(f.session, 1, 2)
	u2, a2 := f.row(f.session, 2, 1), f.row(f.session, 2, 2)
	u3 := f.row(f.session, 3, 1)
	first := f.mustFold(Checkpoint{}, "first", a1)

	if _, err := f.fold(Checkpoint{}, "stale", u1); !errors.Is(err, ErrMoved) {
		t.Fatalf("fold onto a replaced parent = %v, want ErrMoved", err)
	}
	f.exec(`UPDATE bot_history_messages SET turn_visible = false WHERE id = $1`, u2)
	if _, err := f.fold(first, "hidden", u2, a2); !errors.Is(err, ErrMoved) {
		t.Fatalf("fold of a hidden row = %v, want ErrMoved", err)
	}
	f.exec(`UPDATE bot_history_messages SET turn_visible = false WHERE id = $1`, u1)
	if _, err := f.fold(first, "broken parent", u3); !errors.Is(err, ErrMoved) {
		t.Fatalf("fold onto a parent that keeps a removed row = %v, want ErrMoved", err)
	}
	if got := f.pointer(f.session); got != first.ID {
		t.Fatalf("active = %q, want %q", got, first.ID)
	}
}

// Active replaces an active checkpoint that no longer holds with the newest
// one on its chain that does, and returns that one. The repair sticks: the
// next read finds it holding.
func TestPostgresActiveRepairsACheckpointThatNoLongerHolds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		breaks func(f *historyFixture, first, second Checkpoint, kept string)
		// held is the checkpoint left active: "first" or "" for none.
		held string
	}{
		{
			name: "a row it keeps is no longer context",
			breaks: func(f *historyFixture, _, _ Checkpoint, kept string) {
				f.exec(`UPDATE bot_history_messages SET turn_visible = false WHERE id = $1`, kept)
			},
			held: "first",
		},
		{
			name: "its summary is no longer usable",
			breaks: func(f *historyFixture, _, second Checkpoint, _ string) {
				f.exec(`UPDATE bot_history_message_compacts SET status = 'error' WHERE id = $1`, second.SummaryIDs()[0])
			},
			held: "first",
		},
		{
			name: "no checkpoint on its chain holds",
			breaks: func(f *historyFixture, first, second Checkpoint, _ string) {
				f.exec(`DELETE FROM bot_history_message_compacts WHERE id = ANY($1::uuid[])`, append(first.SummaryIDs(), second.SummaryIDs()...))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHistoryFixture(t)
			u1, a1 := f.row(f.session, 1, 1), f.row(f.session, 1, 2)
			u2, a2 := f.row(f.session, 2, 1), f.row(f.session, 2, 2)
			f.row(f.session, 3, 1)
			first := f.mustFold(Checkpoint{}, "first", u1, a1)
			second := f.mustFold(first, "second", a2)
			if !slices.Contains(second.RowIDs(), u2) {
				t.Fatalf("second items = %+v, want %s kept", second.Items, u2)
			}
			tc.breaks(f, first, second, u2)

			want := map[string]string{"first": first.ID, "": ""}[tc.held]
			if got := f.active(); got.ID != want {
				t.Fatalf("Active = %q, want %q", got.ID, want)
			}
			if got := f.pointer(f.session); got != want {
				t.Fatalf("active = %q, want %q", got, want)
			}
			if got := f.active(); got.ID != want {
				t.Fatalf("Active after the repair = %q, want %q", got.ID, want)
			}
		})
	}
}

// A context row that appears at or before the active checkpoint's position,
// written so or made visible there, is recorded as it is written and leaves
// the session where it is: a fold onto that checkpoint is refused, and the
// next read moves the session back to the newest checkpoint before the row,
// and forgets it. A row after the position or a legacy passive_sync row
// moves nothing.
func TestPostgresARowAppearingBelowTheActiveCheckpointMovesItBackOnTheNextRead(t *testing.T) {
	f := newHistoryFixture(t)
	u1, a1 := f.row(f.session, 1, 1), f.row(f.session, 1, 2)
	_, a2 := f.row(f.session, 2, 1), f.row(f.session, 2, 2)
	first := f.mustFold(Checkpoint{}, "first", u1, a1)
	second := f.mustFold(first, "second", a2)

	f.row(f.session, 3, 1)
	f.exec(`INSERT INTO bot_history_messages
		(bot_id, session_id, role, content, metadata, turn_id, turn_position, turn_message_seq, turn_visible)
		VALUES ($1, $2, 'user', '{"role":"user","content":"chatter"}'::jsonb, '{"trigger_mode":"passive_sync"}'::jsonb, $3, 1, 4, true)`,
		f.botID, f.session, f.turns[f.session+"/1"])
	if got := f.appeared(f.session); got != 0 {
		t.Fatalf("rows on record after a row after the position and a passive_sync row below it = %d, want 0", got)
	}

	late := f.row(f.session, 1, 3)
	if got := f.pointer(f.session); got != second.ID {
		t.Fatalf("active as a row is written at 1.3 = %q, want %q", got, second.ID)
	}
	if _, err := f.fold(second, "third"); !errors.Is(err, ErrMoved) {
		t.Fatalf("fold onto a checkpoint a row appeared below = %v, want ErrMoved", err)
	}
	if got := f.active(); got.ID != first.ID {
		t.Fatalf("Active after a row written at 1.3 = %q, want %q", got.ID, first.ID)
	}
	if got, recorded := f.pointer(f.session), f.appeared(f.session); got != first.ID || recorded != 0 {
		t.Fatalf("after the read: active = %q with %d rows on record, want %q with none", got, recorded, first.ID)
	}

	f.exec(`UPDATE bot_sessions SET history_checkpoint_id = $2 WHERE id = $1`, f.session, second.ID)
	f.exec(`UPDATE bot_history_messages SET turn_visible = false WHERE id = $1`, late)
	if got := f.active(); got.ID != second.ID {
		t.Fatalf("Active once the row is hidden = %q, want %q", got.ID, second.ID)
	}
	f.exec(`UPDATE bot_history_messages SET turn_visible = true WHERE id = $1`, late)
	if got := f.active(); got.ID != first.ID {
		t.Fatalf("Active after the row was made visible at 1.3 = %q, want %q", got.ID, first.ID)
	}
}

// Every checkpoint holds a summary: what moves a session back relies on it,
// so the database refuses one without.
func TestPostgresACheckpointAlwaysHoldsASummary(t *testing.T) {
	f := newHistoryFixture(t)
	row := f.row(f.session, 1, 1)
	_, err := create(f.ctx, f.queries, dbpkg.ParseUUIDOrEmpty(f.session), Checkpoint{}, messagepkg.HistoryPosition{Turn: 1, Seq: 1}, items(Item{RowID: row}))
	if err == nil || !strings.Contains(err.Error(), "bot_history_checkpoints_summary_check") {
		t.Fatalf("create without a summary = %v, want the summary check to refuse it", err)
	}
	if got := f.pointer(f.session); got != "" {
		t.Fatalf("active after the refused checkpoint = %q, want none", got)
	}
}

// A read forgets the rows on record only while the checkpoint it found is
// still the session's active one. A row recorded against a checkpoint made
// since stays, and the next read moves the session back before it.
func TestPostgresAReadForgetsNoRowRecordedAfterTheSessionMovedOn(t *testing.T) {
	f := newHistoryFixture(t)
	u1, a1 := f.row(f.session, 1, 1), f.row(f.session, 1, 2)
	u2, a2 := f.row(f.session, 2, 1), f.row(f.session, 2, 2)
	u3, a3 := f.row(f.session, 3, 1), f.row(f.session, 3, 2)
	first := f.mustFold(Checkpoint{}, "first", u1, a1)
	second := f.mustFold(first, "second", u2, a2, u3, a3)
	f.row(f.session, 2, 3)

	forgotten, err := f.queries.ForgetHistoryAppearedRows(f.ctx, dbsqlc.ForgetHistoryAppearedRowsParams{
		SessionID:    dbpkg.ParseUUIDOrEmpty(f.session),
		CheckpointID: dbpkg.ParseUUIDOrEmpty(first.ID),
	})
	if err != nil || forgotten != 0 {
		t.Fatalf("forget for %q once %q is active = %d, %v; want nothing forgotten", first.ID, second.ID, forgotten, err)
	}
	if got := f.active(); got.ID != first.ID {
		t.Fatalf("Active after a row written at 2.3 below %q = %q, want %q", second.ID, got.ID, first.ID)
	}
	if recorded := f.appeared(f.session); recorded != 0 {
		t.Fatalf("rows on record after the read = %d, want none", recorded)
	}
}

// A fenced transaction locks its session before it locks the session for
// update to commit a fold. A row written below the active checkpoint in the
// meantime takes only the session's key share, which the fold waits for and
// which waits on nothing the fold holds.
func TestPostgresARowAppearingWhileAFoldLocksTheSessionDoesNotDeadlock(t *testing.T) {
	f := newHistoryFixture(t)
	u1, a1 := f.row(f.session, 1, 1), f.row(f.session, 1, 2)
	u2, a2 := f.row(f.session, 2, 1), f.row(f.session, 2, 2)
	first := f.mustFold(Checkpoint{}, "first", u1, a1, u2, a2)

	fence, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin fenced transaction: %v", err)
	}
	defer func() { _ = fence.Rollback(context.Background()) }()
	if _, err := fence.Exec(f.ctx, `SELECT id FROM bot_sessions WHERE id = $1 FOR NO KEY UPDATE`, f.session); err != nil {
		t.Fatalf("lock the session as the fence does: %v", err)
	}

	writer, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin writer: %v", err)
	}
	defer func() { _ = writer.Rollback(context.Background()) }()
	written := make(chan error, 1)
	go func() {
		_, err := writer.Exec(f.ctx, `INSERT INTO bot_history_messages
			(bot_id, session_id, role, content, turn_id, turn_position, turn_message_seq, turn_visible)
			VALUES ($1, $2, 'assistant', '{"role":"assistant","content":"late"}'::jsonb, $3, 1, 3, true)`,
			f.botID, f.session, f.turns[f.session+"/1"])
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("write a row below the active checkpoint: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a row written below the active checkpoint waits on the fenced transaction")
	}

	locked := make(chan error, 1)
	go func() {
		_, err := fence.Exec(f.ctx, `SELECT id FROM bot_sessions WHERE id = $1 FOR UPDATE`, f.session)
		locked <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if err := writer.Commit(f.ctx); err != nil {
		t.Fatalf("commit the writer: %v", err)
	}
	select {
	case err := <-locked:
		if err != nil {
			t.Fatalf("lock the session for update after the writer: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the fenced transaction still waits after the writer committed")
	}
	if err := fence.Commit(f.ctx); err != nil {
		t.Fatalf("commit the fenced transaction: %v", err)
	}
	if got := f.active(); got.ID != "" {
		t.Fatalf("Active after a row written at 1.3 below %q = %q, want none", first.ID, got.ID)
	}
}

// An asset linked to a row a checkpoint folds, or linked again with other
// details, changes no context row: the session keeps its checkpoint, and the
// next fold commits onto it.
func TestPostgresAnAssetLinkedToAFoldedRowLeavesTheCheckpoint(t *testing.T) {
	f := newHistoryFixture(t)
	u1, a1 := f.row(f.session, 1, 1), f.row(f.session, 1, 2)
	_, a2 := f.row(f.session, 2, 1), f.row(f.session, 2, 2)
	first := f.mustFold(Checkpoint{}, "first", u1, a1)

	answer, err := dbpkg.ParseUUID(a1)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"chart.png", "chart-final.png"} {
		if _, err := f.queries.CreateMessageAsset(f.ctx, dbsqlc.CreateMessageAssetParams{
			MessageID:   answer,
			Role:        "attachment",
			ContentHash: "chart",
			Mime:        "image/png",
			SizeBytes:   1024,
			StorageKey:  "media/chart.png",
			Name:        name,
			Metadata:    []byte(`{}`),
		}); err != nil {
			t.Fatalf("link %s to the folded answer: %v", name, err)
		}
		if got, recorded := f.active(), f.appeared(f.session); got.ID != first.ID || recorded != 0 {
			t.Fatalf("Active after linking %s = %q with %d rows on record, want %q with none", name, got.ID, recorded, first.ID)
		}
	}
	if _, err := f.fold(first, "second", a2); err != nil {
		t.Fatalf("fold onto the checkpoint after the links: %v", err)
	}
}

// What an edit or retry renders before its replacement commits is the
// checkpoint the session keeps once the turn goes: the newest one before
// the turn. Reading it leaves the session as it is; rewinding before the turn
// makes it active.
func TestPostgresKeptBeforeTurnIsWhatAReplacementKeeps(t *testing.T) {
	f := newHistoryFixture(t)
	u1, a1 := f.row(f.session, 1, 1), f.row(f.session, 1, 2)
	u2, a2 := f.row(f.session, 2, 1), f.row(f.session, 2, 2)
	f.row(f.session, 3, 1)
	first := f.mustFold(Checkpoint{}, "first", u1, a1)
	second := f.mustFold(first, "second", u2, a2)
	turn := func(position int64) string { return f.turns[fmt.Sprintf("%s/%d", f.session, position)] }

	for _, tc := range []struct {
		turn int64
		want string
	}{
		{turn: 3, want: second.ID},
		{turn: 2, want: first.ID},
		{turn: 1, want: ""},
	} {
		kept, err := f.store.KeptBeforeTurn(f.ctx, f.session, turn(tc.turn))
		if err != nil {
			t.Fatalf("KeptBeforeTurn(%d): %v", tc.turn, err)
		}
		if kept.ID != tc.want {
			t.Fatalf("KeptBeforeTurn(%d) = %q, want %q", tc.turn, kept.ID, tc.want)
		}
	}
	if got := f.pointer(f.session); got != second.ID {
		t.Fatalf("active after reads = %q, want %q", got, second.ID)
	}

	rewind := func() bool {
		t.Helper()
		var rewound bool
		if err := f.queries.InTx(f.ctx, func(q dbstore.Queries) error {
			var err error
			rewound, err = q.RewindHistoryCheckpointBeforeTurn(f.ctx, dbsqlc.RewindHistoryCheckpointBeforeTurnParams{
				SessionID: dbpkg.ParseUUIDOrEmpty(f.session),
				BotID:     dbpkg.ParseUUIDOrEmpty(f.botID),
				TurnID:    dbpkg.ParseUUIDOrEmpty(turn(2)),
			})
			return err
		}); err != nil {
			t.Fatalf("rewind: %v", err)
		}
		return rewound
	}
	if !rewind() || f.pointer(f.session) != first.ID {
		t.Fatalf("rewind before turn 2 left %q active, want %q", f.pointer(f.session), first.ID)
	}
	if rewind() {
		t.Fatal("a second rewind before turn 2 reported a move")
	}
}

// A fork carries the newest checkpoint of its source's chain that accounts
// for no history past the copy and keeps only rows it copied: its summaries
// become the fork's own, its rows are named by their copies, and it sits at
// the copy's place for its position, with no parent.
func TestPostgresCopyToForkCarriesTheNewestCheckpointTheCopyHolds(t *testing.T) {
	f := newHistoryFixture(t)
	turn := func(position int64) [2]string {
		return [2]string{f.row(f.session, position, 1), f.row(f.session, position, 2)}
	}
	source := [4][2]string{turn(1), turn(2), turn(3), turn(4)}
	first := f.mustFold(Checkpoint{}, "first", source[0][1])
	second := f.mustFold(first, "second", source[0][0], source[1][0], source[1][1], source[2][0])
	if !slices.Contains(first.RowIDs(), source[0][0]) || len(second.Items) != 1 {
		t.Fatalf("chain = %+v / %+v, want the first keeping its user row and the second only a summary", first.Items, second.Items)
	}

	// fork copies the source's rows of the given turns into a new session,
	// ten turns later, as a thread fork binds its copies.
	fork := func(throughTurn int, skip ...string) (string, Copy, map[string]string) {
		session := f.newSession()
		copies := make(map[string]string)
		c := Copy{SourceSessionID: f.session, ForkSessionID: session, ThroughTurnPosition: int64(throughTurn)}
		for turn := 0; turn < throughTurn; turn++ {
			for seq, id := range source[turn] {
				if slices.Contains(skip, id) {
					continue
				}
				copied := f.row(session, int64(turn+11), int64(seq+1))
				copies[id] = copied
				c.SourceMessageIDs = append(c.SourceMessageIDs, dbpkg.ParseUUIDOrEmpty(id))
				c.ForkMessageIDs = append(c.ForkMessageIDs, dbpkg.ParseUUIDOrEmpty(copied))
			}
		}
		return session, c, copies
	}
	copyToFork := func(c Copy) Checkpoint {
		t.Helper()
		var copied Checkpoint
		if err := f.queries.InTx(f.ctx, func(q dbstore.Queries) error {
			var err error
			copied, err = CopyToFork(f.ctx, q, c)
			return err
		}); err != nil {
			t.Fatalf("copy to fork: %v", err)
		}
		return copied
	}

	session, c, copies := fork(2)
	copied := copyToFork(c)
	if copied.ParentID != "" || copied.SessionID != session || copied.Position != (messagepkg.HistoryPosition{Turn: 11, Seq: 2}) {
		t.Fatalf("copy = session %q parent %q at %+v, want %q without parent at 11.2", copied.SessionID, copied.ParentID, copied.Position, session)
	}
	if len(copied.Items) != 2 || copied.Items[0].RowID != copies[source[0][0]] || copied.Items[1].SummaryID == "" || copied.Items[1].SummaryID == first.SummaryIDs()[0] {
		t.Fatalf("copy items = %+v, want the copied row then a summary of the fork's own", copied.Items)
	}
	if got := f.pointer(session); got != copied.ID {
		t.Fatalf("fork active = %q, want %q", got, copied.ID)
	}
	f.session = session
	if got := f.active(); got.ID != copied.ID || got.Items[1].Summary != "first" {
		t.Fatalf("fork Active = %+v, want %q holding the source's summary text", got, copied.ID)
	}
	f.session = c.SourceSessionID
	if got := f.pointer(f.session); got != second.ID {
		t.Fatalf("source active = %q, want %q", got, second.ID)
	}

	if _, c, _ = fork(3); copyToFork(c).Position != (messagepkg.HistoryPosition{Turn: 13, Seq: 1}) {
		t.Fatal("a fork through turn 3 did not carry the checkpoint through 3.1")
	}
	if session, c, _ = fork(2, source[0][0]); copyToFork(c).ID != "" || f.pointer(session) != "" {
		t.Fatal("a fork that did not copy a kept row carried a checkpoint")
	}
}

// Checkpoints go with their session; one removed under the session leaves it
// with the history it rendered raw, and its descendants go with it.
func TestPostgresCheckpointsGoWithTheirSession(t *testing.T) {
	f := newHistoryFixture(t)
	u1, a1 := f.row(f.session, 1, 1), f.row(f.session, 1, 2)
	u2 := f.row(f.session, 2, 1)
	first := f.mustFold(Checkpoint{}, "first", u1, a1)
	second := f.mustFold(first, "second", u2)

	f.exec(`DELETE FROM bot_history_checkpoints WHERE id = $1`, first.ID)
	var left int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM bot_history_checkpoints WHERE id = ANY($1::uuid[])`, []string{first.ID, second.ID}).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 || f.pointer(f.session) != "" {
		t.Fatalf("after removing the root: %d checkpoints left, active %q; want none", left, f.pointer(f.session))
	}

	third := f.mustFold(Checkpoint{}, "third", u1)
	f.exec(`DELETE FROM bot_sessions WHERE id = $1`, f.session)
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM bot_history_checkpoints WHERE id = $1`, third.ID).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatal("a removed session left its checkpoint")
	}
}
