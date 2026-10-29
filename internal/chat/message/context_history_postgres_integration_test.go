package message

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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

func persistRefusedContextHistoryRow(t *testing.T, ctx context.Context, svc *DBService, sessionID, role, text string) string {
	t.Helper()
	msg, err := svc.Persist(ctx, PersistInput{
		BotID: postgresMessageTestBotID, SessionID: sessionID, Role: role,
		Content:   []byte(`{"role":"` + role + `","content":"` + text + `"}`),
		Admission: AdmissionRejectedACL,
	})
	if err != nil {
		t.Fatalf("persist refused %s row: %v", text, err)
	}
	return msg.ID
}

// Rows refused at admission are left out before the limit, so the newest N
// admitted rows come back even when the newest rows of all are refused.
func TestPostgresListRecentUserBySessionKeepsItsPlacesForAdmittedRows(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)
	svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))
	sid := postgresMessageTestSessionID

	persistContextHistoryRow(t, ctx, svc, sid, "user", "u1")
	u2 := persistContextHistoryRow(t, ctx, svc, sid, "user", "u2")
	u3 := persistContextHistoryRow(t, ctx, svc, sid, "user", "u3")
	persistRefusedContextHistoryRow(t, ctx, svc, sid, "user", "refused-1")
	persistRefusedContextHistoryRow(t, ctx, svc, sid, "user", "refused-2")

	msgs, err := svc.ListRecentUserBySession(ctx, sid, 2)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(msgs))
	for _, m := range msgs {
		got = append(got, m.ID)
	}
	assertContextHistoryIDs(t, got, u2, u3)
}

// The bounded compaction read and the measure beside it leave refused rows out
// together, so neither their count nor their bytes use up the read budget.
// The include_unadmitted argument turns the exclusion off.
func TestPostgresCompactionCandidatesExcludeRefusedRows(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)
	svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))
	sid := postgresMessageTestSessionID

	u1 := persistContextHistoryRow(t, ctx, svc, sid, "user", "u1")
	persistRefusedContextHistoryRow(t, ctx, svc, sid, "user", "refused-1")
	a1 := persistContextHistoryRow(t, ctx, svc, sid, "assistant", "a1")
	persistRefusedContextHistoryRow(t, ctx, svc, sid, "user", "refused-2")

	queries := dbsqlc.New(tx)
	sessionID := mustTestUUID(t, sid)
	for _, c := range []struct {
		name    string
		include bool
		want    int
	}{
		{"excluded", false, 2},
		{"included", true, 4},
	} {
		rows, err := queries.ListUncompactedMessagesBySessionWithinBytes(ctx, dbsqlc.ListUncompactedMessagesBySessionWithinBytesParams{
			SessionID: sessionID, IncludeUnadmitted: c.include, MaxBytes: 1 << 30,
		})
		if err != nil {
			t.Fatalf("%s: list candidates: %v", c.name, err)
		}
		measure, err := queries.MeasureUncompactedMessagesBySession(ctx, dbsqlc.MeasureUncompactedMessagesBySessionParams{
			SessionID: sessionID, IncludeUnadmitted: c.include,
		})
		if err != nil {
			t.Fatalf("%s: measure candidates: %v", c.name, err)
		}
		if len(rows) != c.want || measure.CandidateCount != int64(c.want) {
			t.Fatalf("%s: listed %d rows, measured %d, want %d", c.name, len(rows), measure.CandidateCount, c.want)
		}
		if !c.include {
			assertContextHistoryIDs(t, []string{uuidString(rows[0].ID), uuidString(rows[1].ID)}, u1, a1)
		}
	}
}

// Rows refused at admission use none of the byte budget: refused rows larger
// than the whole budget, both newer and older than the readable rows, leave
// those rows in the window. Measuring the budget before leaving them out would
// let the newest refused row spend it all and return nothing readable.
func TestPostgresListActiveAfterCompactionBudgetIgnoresRefusedRows(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)
	svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))
	sid := postgresMessageTestSessionID
	huge := strings.Repeat("x", 8000)

	persistRefusedContextHistoryRow(t, ctx, svc, sid, "user", huge)
	h1 := persistContextHistoryRow(t, ctx, svc, sid, "user", "h1")
	persistRefusedContextHistoryRow(t, ctx, svc, sid, "user", huge)
	h2 := persistContextHistoryRow(t, ctx, svc, sid, "assistant", "h2")
	h3 := persistContextHistoryRow(t, ctx, svc, sid, "user", "h3")
	persistRefusedContextHistoryRow(t, ctx, svc, sid, "user", huge)

	var readableBytes int64
	if err := tx.QueryRow(ctx, `SELECT sum(octet_length(content::text)) FROM bot_history_messages WHERE id = ANY($1::uuid[])`,
		[]string{h1, h2, h3}).Scan(&readableBytes); err != nil {
		t.Fatalf("measure readable rows: %v", err)
	}

	history, err := svc.ListActiveAfterCompactionBySessionWithinBytes(ctx, sid, readableBytes)
	if err != nil {
		t.Fatal(err)
	}
	assertContextHistoryIDs(t, contextHistoryIDs(history), h1, h2, h3)

	rows, err := dbsqlc.New(tx).ListActiveMessagesAfterCompactionBySessionWithinBytes(ctx, dbsqlc.ListActiveMessagesAfterCompactionBySessionWithinBytesParams{
		SessionID: mustTestUUID(t, sid), IncludeUnadmitted: true, MaxBytes: readableBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("include_unadmitted read %d rows under the same budget, want only the newest refused row", len(rows))
	}
}

// The readers behind the history tools leave refused rows out before any row
// limit and report one by id as absent, while the plain readers still return it.
func TestPostgresForModelReadersLeaveOutRefusedRows(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)
	svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))
	sid := postgresMessageTestSessionID

	u1 := persistContextHistoryRow(t, ctx, svc, sid, "user", "u1")
	refused := persistRefusedContextHistoryRow(t, ctx, svc, sid, "user", "refused")
	a1 := persistContextHistoryRow(t, ctx, svc, sid, "assistant", "a1")
	later := time.Now().Add(time.Hour)

	ids := func(msgs []Message) []string {
		out := make([]string, 0, len(msgs))
		for _, m := range msgs {
			out = append(out, m.ID)
		}
		return out
	}
	sorted := func(values ...string) string {
		slices.Sort(values)
		return strings.Join(values, ",")
	}
	assertSet := func(t *testing.T, name string, got []string, want ...string) {
		t.Helper()
		if sorted(got...) != sorted(want...) {
			t.Fatalf("%s = %v, want %v in any order", name, got, want)
		}
	}

	t.Run("latest", func(t *testing.T) {
		plain, err := svc.ListLatestBySession(ctx, sid, 2)
		if err != nil {
			t.Fatal(err)
		}
		assertSet(t, "plain", ids(plain), a1, refused)
		forModel, err := svc.ListLatestForModelBySession(ctx, sid, 2)
		if err != nil {
			t.Fatal(err)
		}
		assertSet(t, "for model", ids(forModel), a1, u1)
	})
	t.Run("before", func(t *testing.T) {
		plain, err := svc.ListBeforeBySession(ctx, sid, later, 2)
		if err != nil {
			t.Fatal(err)
		}
		assertSet(t, "plain", ids(plain), a1, refused)
		forModel, err := svc.ListBeforeForModelBySession(ctx, sid, later, 2)
		if err != nil {
			t.Fatal(err)
		}
		assertSet(t, "for model", ids(forModel), a1, u1)
	})
	t.Run("by id", func(t *testing.T) {
		plain, err := svc.GetByIDBySession(ctx, sid, refused)
		if err != nil || plain.ID != refused {
			t.Fatalf("plain reader = (%q, %v), want the refused row", plain.ID, err)
		}
		if _, err := svc.GetByIDForModelBySession(ctx, sid, refused); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("for model reader error = %v, want pgx.ErrNoRows", err)
		}
		admitted, err := svc.GetByIDForModelBySession(ctx, sid, u1)
		if err != nil || admitted.ID != u1 {
			t.Fatalf("for model reader = (%q, %v), want the admitted row", admitted.ID, err)
		}
	})
	t.Run("visible from", func(t *testing.T) {
		plain, err := svc.ListVisibleFromBySession(ctx, sid, u1)
		if err != nil {
			t.Fatal(err)
		}
		assertSet(t, "plain", ids(plain), u1, refused, a1)
		forModel, err := svc.ListVisibleFromForModelBySession(ctx, sid, u1)
		if err != nil {
			t.Fatal(err)
		}
		assertSet(t, "for model", ids(forModel), u1, a1)
		// A refused row that starts the window is left out of it as well.
		fromRefused, err := svc.ListVisibleFromForModelBySession(ctx, sid, refused)
		if err != nil {
			t.Fatal(err)
		}
		assertSet(t, "for model from the refused row", ids(fromRefused), a1)
	})
}

// A row written without naming an admission, which is every row that existed
// before the column, is admitted.
func TestPostgresRowWithoutAdmissionReadsAsAdmitted(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)
	svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))
	sid := postgresMessageTestSessionID

	var legacyID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO bot_history_messages (bot_id, session_id, role, content, turn_id, turn_position, turn_message_seq, turn_visible)
		VALUES ($1, $2, 'user', '{"role":"user","content":"legacy"}'::jsonb, gen_random_uuid(), 0, 1, true)
		RETURNING id::text`, postgresMessageTestBotID, sid).Scan(&legacyID); err != nil {
		t.Fatalf("insert row without admission: %v", err)
	}
	var stored string
	if err := tx.QueryRow(ctx, `SELECT admission FROM bot_history_messages WHERE id = $1`, legacyID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != string(AdmissionAdmitted) {
		t.Fatalf("stored admission = %q, want %q", stored, AdmissionAdmitted)
	}
	persistRefusedContextHistoryRow(t, ctx, svc, sid, "user", "refused")

	msgs, err := svc.ListRecentUserBySession(ctx, sid, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(msgs))
	for _, m := range msgs {
		got = append(got, m.ID)
	}
	assertContextHistoryIDs(t, got, legacyID)
}

// The column refuses a value the gates do not write, so a mistyped admission
// fails the write instead of reading as admitted or as refused by accident.
func TestPostgresUnknownAdmissionIsRefusedAtWrite(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)
	svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))

	_, err := svc.Persist(ctx, PersistInput{
		BotID: postgresMessageTestBotID, SessionID: postgresMessageTestSessionID, Role: "user",
		Content:   []byte(`{"role":"user","content":"x"}`),
		Admission: Admission("refused"),
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "bot_history_messages_admission_check" {
		t.Fatalf("persist with unknown admission = %v, want the admission CHECK violation", err)
	}
}

// A row costs the load what history puts in front of the model for it: its
// content and the runtime context its request recorded. The process counts
// the same bytes from what the load hands over.
func TestPostgresListActiveAfterCompactionChargesRuntimeContext(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)
	svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))
	sid := postgresMessageTestSessionID

	var ids []string
	for _, text := range []string{"oldest", "request", "newest"} {
		ids = append(ids, persistContextHistoryRow(t, ctx, svc, sid, "user", text))
	}
	if _, err := tx.Exec(ctx, `
		UPDATE bot_history_messages
		SET metadata = metadata || jsonb_build_object('runtime_context', jsonb_build_array(jsonb_build_object(
			'kind', 'memory_recall',
			'message', jsonb_build_object('role', 'user', 'content', repeat('recalled ', 200)))))
		WHERE id = $1`, ids[1]); err != nil {
		t.Fatalf("record runtime context: %v", err)
	}
	var contentBytes, chargedBytes int64
	if err := tx.QueryRow(ctx, `
		SELECT sum(octet_length(content::text)),
		       sum(octet_length(content::text) + COALESCE(octet_length((metadata -> 'runtime_context')::text), 0))
		FROM bot_history_messages WHERE id = ANY($1::uuid[])`, ids[1:]).Scan(&contentBytes, &chargedBytes); err != nil {
		t.Fatalf("measure rows: %v", err)
	}
	if chargedBytes <= contentBytes {
		t.Fatalf("charged %d bytes, content %d: the fixture records no runtime context", chargedBytes, contentBytes)
	}

	// The two newest rows use up the budget once the runtime context counts;
	// by their content alone the oldest row would still fit.
	history, err := svc.ListActiveAfterCompactionBySessionWithinBytes(ctx, sid, chargedBytes)
	if err != nil {
		t.Fatal(err)
	}
	assertContextHistoryIDs(t, contextHistoryIDs(history), ids[1], ids[2])
	var counted int64
	for _, message := range history.Messages {
		counted += message.ContextBytes()
	}
	if counted != chargedBytes {
		t.Fatalf("process counted %d bytes, the load charged %d", counted, chargedBytes)
	}
}
