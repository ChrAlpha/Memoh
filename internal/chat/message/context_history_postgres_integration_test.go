package message

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

const (
	contextHistoryOtherSessionID = "00000000-0000-0000-0000-000000074104"
	contextHistoryOtherBotID     = "00000000-0000-0000-0000-000000074105"
	contextHistoryOtherBotSessID = "00000000-0000-0000-0000-000000074106"
)

func persistContextHistoryRow(t *testing.T, ctx context.Context, svc *DBService, sessionID, role, text string) string {
	t.Helper()
	botID := postgresMessageTestBotID
	if sessionID == contextHistoryOtherBotSessID {
		botID = contextHistoryOtherBotID
	}
	msg, err := svc.Persist(ctx, PersistInput{
		BotID: botID, SessionID: sessionID, Role: role,
		Content: []byte(`{"role":"` + role + `","content":"` + text + `"}`),
	})
	if err != nil {
		t.Fatalf("persist %s row: %v", text, err)
	}
	return msg.ID
}

func setupContextHistoryExtraFixtures(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	if _, err := tx.Exec(ctx, `INSERT INTO bot_sessions (id, bot_id, channel_type) VALUES ($1, $2, 'local')`, contextHistoryOtherSessionID, postgresMessageTestBotID); err != nil {
		t.Fatalf("insert second session: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO bots (id, owner_user_id, name) VALUES ($1, $2, 'context-history-other-bot')`, contextHistoryOtherBotID, postgresMessageTestUserID); err != nil {
		t.Fatalf("insert second bot: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO bot_sessions (id, bot_id, channel_type) VALUES ($1, $2, 'local')`, contextHistoryOtherBotSessID, contextHistoryOtherBotID); err != nil {
		t.Fatalf("insert second bot session: %v", err)
	}
}

func insertContextHistoryEvent(t *testing.T, ctx context.Context, tx pgx.Tx, botID, sessionID, externalID string) {
	t.Helper()
	if _, err := tx.Exec(ctx, `
		INSERT INTO bot_session_events (bot_id, session_id, event_kind, event_data, external_message_id, received_at_ms)
		VALUES ($1, $2, 'message', '{}'::jsonb, $3, 1)`, botID, sessionID, externalID); err != nil {
		t.Fatalf("insert session event: %v", err)
	}
}

func countContextHistoryEvents(t *testing.T, ctx context.Context, tx pgx.Tx, sessionID string) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM bot_session_events WHERE session_id = $1`, sessionID).Scan(&n); err != nil {
		t.Fatalf("count session events: %v", err)
	}
	return n
}

func TestPostgresClearHistoryDeletesSessionEvents(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)
	setupContextHistoryExtraFixtures(t, ctx, tx)
	svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))
	insertContextHistoryEvent(t, ctx, tx, postgresMessageTestBotID, postgresMessageTestSessionID, "a")
	insertContextHistoryEvent(t, ctx, tx, postgresMessageTestBotID, contextHistoryOtherSessionID, "b")
	insertContextHistoryEvent(t, ctx, tx, contextHistoryOtherBotID, contextHistoryOtherBotSessID, "c")

	if err := svc.DeleteBySession(ctx, postgresMessageTestSessionID); err != nil {
		t.Fatalf("DeleteBySession: %v", err)
	}
	if got := countContextHistoryEvents(t, ctx, tx, postgresMessageTestSessionID); got != 0 {
		t.Fatalf("cleared session still has %d events", got)
	}
	if got := countContextHistoryEvents(t, ctx, tx, contextHistoryOtherSessionID); got != 1 {
		t.Fatalf("sibling session events = %d, want 1", got)
	}
	if got := countContextHistoryEvents(t, ctx, tx, contextHistoryOtherBotSessID); got != 1 {
		t.Fatalf("other bot events = %d, want 1", got)
	}

	if err := svc.DeleteByBot(ctx, postgresMessageTestBotID); err != nil {
		t.Fatalf("DeleteByBot: %v", err)
	}
	if got := countContextHistoryEvents(t, ctx, tx, contextHistoryOtherSessionID); got != 0 {
		t.Fatalf("cleared bot still has %d events", got)
	}
	if got := countContextHistoryEvents(t, ctx, tx, contextHistoryOtherBotSessID); got != 1 {
		t.Fatalf("other bot events = %d, want 1", got)
	}
}

func contextHistoryIDs(history SessionHistoryAfterCompaction) []string {
	ids := make([]string, 0, len(history.Messages))
	for _, m := range history.Messages {
		ids = append(ids, m.ID)
	}
	return ids
}

func assertContextHistoryIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("message ids = %v, want %v", got, want)
	}
}

func TestPostgresListActiveAfterCompactionHasNoTimeWindow(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)
	svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))
	sid := postgresMessageTestSessionID

	covered := persistContextHistoryRow(t, ctx, svc, sid, "user", "covered")
	uncoveredOld := persistContextHistoryRow(t, ctx, svc, sid, "assistant", "uncovered-old")
	pendingCovered := persistContextHistoryRow(t, ctx, svc, sid, "user", "pending-claimed")
	after := persistContextHistoryRow(t, ctx, svc, sid, "assistant", "after-boundary")
	if _, err := tx.Exec(ctx, `UPDATE bot_history_messages SET created_at = now() - interval '25 hours' WHERE id = ANY($1::uuid[])`,
		[]string{covered, uncoveredOld, pendingCovered}); err != nil {
		t.Fatalf("age rows: %v", err)
	}

	var okID, pendingID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO bot_history_message_compacts (bot_id, session_id, status, summary, anchor_start_ms, anchor_end_ms, completed_at)
		VALUES ($1, $2, 'ok', 'the summary', 1, 2, now()) RETURNING id`, postgresMessageTestBotID, sid).Scan(&okID); err != nil {
		t.Fatalf("insert ok artifact: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO bot_history_message_compacts (bot_id, session_id, status)
		VALUES ($1, $2, 'pending') RETURNING id`, postgresMessageTestBotID, sid).Scan(&pendingID); err != nil {
		t.Fatalf("insert pending artifact: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE bot_history_messages SET compact_id = $2 WHERE id = $1`, covered, okID); err != nil {
		t.Fatalf("claim covered row: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE bot_history_messages SET compact_id = $2 WHERE id = $1`, pendingCovered, pendingID); err != nil {
		t.Fatalf("claim pending row: %v", err)
	}

	history, err := svc.ListActiveAfterCompactionBySessionWithinBytes(ctx, sid, 1<<20)
	if err != nil {
		t.Fatalf("ListActiveAfterCompactionBySessionWithinBytes: %v", err)
	}
	assertContextHistoryIDs(t, contextHistoryIDs(history), uncoveredOld, pendingCovered, after)
	if len(history.Summaries) != 1 || history.Summaries[0].ID != okID || history.Summaries[0].Summary != "the summary" {
		t.Fatalf("summaries = %+v, want only the ok artifact %s", history.Summaries, okID)
	}
}

func TestPostgresListActiveAfterCompactionStopsAtByteBudget(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)
	svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))
	sid := postgresMessageTestSessionID

	var ids []string
	for _, text := range []string{"oldest", "older", "newer", "newest"} {
		ids = append(ids, persistContextHistoryRow(t, ctx, svc, sid, "user", text))
	}
	var newestThreeBytes int64
	if err := tx.QueryRow(ctx, `SELECT sum(octet_length(content::text)) FROM bot_history_messages WHERE id = ANY($1::uuid[])`, ids[1:]).Scan(&newestThreeBytes); err != nil {
		t.Fatalf("measure rows: %v", err)
	}

	// Budget below one row: the crossing row is still kept, so the newest loads.
	history, err := svc.ListActiveAfterCompactionBySessionWithinBytes(ctx, sid, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertContextHistoryIDs(t, contextHistoryIDs(history), ids[3])

	// Budget equal to the three newest rows: admission stops once the running
	// total reaches it, so the oldest row stays out.
	history, err = svc.ListActiveAfterCompactionBySessionWithinBytes(ctx, sid, newestThreeBytes)
	if err != nil {
		t.Fatal(err)
	}
	assertContextHistoryIDs(t, contextHistoryIDs(history), ids[1], ids[2], ids[3])

	history, err = svc.ListActiveAfterCompactionBySessionWithinBytes(ctx, sid, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	assertContextHistoryIDs(t, contextHistoryIDs(history), ids...)
}

func TestPostgresListRecentUserBySession(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)
	setupContextHistoryExtraFixtures(t, ctx, tx)
	svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))
	sid := postgresMessageTestSessionID

	u1 := persistContextHistoryRow(t, ctx, svc, sid, "user", "u1")
	persistContextHistoryRow(t, ctx, svc, sid, "assistant", "a1")
	u2 := persistContextHistoryRow(t, ctx, svc, sid, "user", "u2")
	persistContextHistoryRow(t, ctx, svc, contextHistoryOtherSessionID, "user", "other-session")
	u3 := persistContextHistoryRow(t, ctx, svc, sid, "user", "u3")
	persistContextHistoryRow(t, ctx, svc, sid, "assistant", "a2")

	msgs, err := svc.ListRecentUserBySession(ctx, sid, 2)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if m.Role != "user" {
			t.Fatalf("non-user row returned: %+v", m)
		}
		got = append(got, m.ID)
	}
	assertContextHistoryIDs(t, got, u2, u3)

	msgs, err = svc.ListRecentUserBySession(ctx, sid, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 || msgs[0].ID != u1 {
		t.Fatalf("limit above row count = %d rows, first %q, want 3 rows starting at %s", len(msgs), msgs[0].ID, u1)
	}
}
