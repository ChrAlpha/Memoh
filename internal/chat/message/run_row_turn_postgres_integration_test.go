package message

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

type runRowTurnFixture struct {
	pool      *pgxpool.Pool
	service   *DBService
	botID     string
	sessionID string
	// older and latest are the request rows of the session's two visible turns.
	older, latest Message
}

func newRunRowTurnFixture(t *testing.T) runRowTurnFixture {
	t.Helper()
	ctx := context.Background()
	pool := openRuntimeFencePostgresPool(t, ctx)
	botID, sessionID := createRuntimeFenceFixtures(t, ctx, pool)
	f := runRowTurnFixture{
		pool:      pool,
		service:   NewService(nil, postgresstore.NewQueriesWithPool(pool, dbsqlc.New(pool))),
		botID:     botID.String(),
		sessionID: sessionID.String(),
	}
	var senderID string
	if err := pool.QueryRow(ctx, "SELECT owner_user_id::text FROM bots WHERE id = $1", botID).Scan(&senderID); err != nil {
		t.Fatalf("load sender: %v", err)
	}
	turn := func(text string) Message {
		user, err := f.service.Persist(ctx, PersistInput{
			BotID: f.botID, SessionID: f.sessionID, Role: "user", SenderUserID: senderID,
			Content: []byte(`{"role":"user","content":"` + text + `"}`),
		})
		if err != nil {
			t.Fatalf("persist user: %v", err)
		}
		if _, err := f.service.Persist(ctx, PersistInput{
			BotID: f.botID, SessionID: f.sessionID, Role: "assistant", TurnRequestMessageID: user.ID,
			Content: []byte(`{"role":"assistant","content":"answer to ` + text + `"}`),
		}); err != nil {
			t.Fatalf("persist assistant: %v", err)
		}
		return user
	}
	f.older, f.latest = turn("older"), turn("latest")
	return f
}

// snapshot is every history row of the session with the turn and position it
// is filed under.
func (f runRowTurnFixture) snapshot(t *testing.T) []string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT id::text || ':' || COALESCE(turn_id::text, '') || ':' || COALESCE(turn_message_seq, 0)::text || ':' || turn_visible::text
		FROM bot_history_messages WHERE session_id = $1 ORDER BY id`, f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f runRowTurnFixture) runRow(role, requestID string) PersistInput {
	return PersistInput{
		BotID: f.botID, SessionID: f.sessionID, Role: role, RunID: uuid.NewString(),
		TurnRequestMessageID: requestID, Content: []byte(`{"role":"` + role + `","content":"late"}`),
	}
}

// A run's row that names no request row, or whose request row was deleted or
// hidden by a replacement, is reported and writes nothing: no row lands in the
// session's latest turn and no turn is created for it.
func TestPostgresRunRowWithoutResolvableRequestWritesNothing(t *testing.T) {
	for _, role := range []string{"assistant", "tool"} {
		for _, tc := range []struct {
			name  string
			setup func(t *testing.T, f runRowTurnFixture) string
		}{
			{"no request row", func(*testing.T, runRowTurnFixture) string { return "" }},
			{"request row deleted", func(t *testing.T, f runRowTurnFixture) string {
				if _, err := f.pool.Exec(context.Background(), `DELETE FROM bot_history_messages WHERE id = $1`, f.older.ID); err != nil {
					t.Fatal(err)
				}
				return f.older.ID
			}},
			{"request row hidden by a replacement", func(t *testing.T, f runRowTurnFixture) string {
				if _, err := f.pool.Exec(context.Background(), `UPDATE bot_history_messages SET turn_visible = false WHERE id = $1`, f.older.ID); err != nil {
					t.Fatal(err)
				}
				return f.older.ID
			}},
		} {
			t.Run(role+"/"+tc.name, func(t *testing.T) {
				f := newRunRowTurnFixture(t)
				requestID := tc.setup(t, f)
				before := f.snapshot(t)
				_, err := f.service.Persist(context.Background(), f.runRow(role, requestID))
				if !errors.Is(err, ErrRunRowTurnNotFound) {
					t.Fatalf("Persist() error = %v, want ErrRunRowTurnNotFound", err)
				}
				if after := f.snapshot(t); !reflect.DeepEqual(before, after) {
					t.Fatalf("history changed:\nbefore %v\nafter  %v", before, after)
				}
			})
		}
	}
}

// A row that belongs to no run, such as a background task notice, is filed in
// the session's latest visible turn.
func TestPostgresRowOfNoRunJoinsTheLatestTurn(t *testing.T) {
	f := newRunRowTurnFixture(t)
	notice, err := f.service.Persist(context.Background(), PersistInput{
		BotID: f.botID, SessionID: f.sessionID, Role: "assistant",
		Content: []byte(`{"role":"assistant","content":"task finished"}`),
	})
	if err != nil {
		t.Fatalf("Persist() error = %v", err)
	}
	var noticeTurn, latestTurn string
	var seq int64
	if err := f.pool.QueryRow(context.Background(), `SELECT turn_id::text, turn_message_seq FROM bot_history_messages WHERE id = $1`, notice.ID).Scan(&noticeTurn, &seq); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT turn_id::text FROM bot_history_messages WHERE id = $1`, f.latest.ID).Scan(&latestTurn); err != nil {
		t.Fatal(err)
	}
	if noticeTurn != latestTurn || seq != 3 {
		t.Fatalf("notice filed under turn %s seq %d, want latest turn %s seq 3", noticeTurn, seq, latestTurn)
	}
}

// A step that has no request row and writes none itself is refused whole.
func TestPostgresAgentStepWithoutRequestRowWritesNothing(t *testing.T) {
	f := newBoundReplacementFixture(t, PersistInput{})
	count := func() int {
		var n int
		if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM bot_history_messages WHERE session_id = $1`, f.sessionID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	_, err := f.service.PersistAgentStep(f.owner, AgentStep{RunID: f.runID, Messages: []PersistInput{
		f.input("assistant", `{"role":"assistant","content":"first"}`, ""),
		f.input("assistant", `{"role":"assistant","content":"second"}`, ""),
	}})
	if !errors.Is(err, ErrRunRowTurnNotFound) {
		t.Fatalf("PersistAgentStep() error = %v, want ErrRunRowTurnNotFound", err)
	}
	if after := count(); after != before {
		t.Fatalf("history rows %d -> %d, want unchanged", before, after)
	}
}
