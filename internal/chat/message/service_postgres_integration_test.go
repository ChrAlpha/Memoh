package message

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	dbpkg "github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

func TestPostgresReplaceTurnRetryHidesSupersededAssistant(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)

	svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))
	user, err := svc.Persist(ctx, PersistInput{
		BotID:     postgresMessageTestBotID,
		SessionID: postgresMessageTestSessionID,
		Role:      "user",
		Content:   []byte(`{"role":"user","content":"hello"}`),
	})
	if err != nil {
		t.Fatalf("persist user: %v", err)
	}
	assistant, err := svc.Persist(ctx, PersistInput{
		BotID:     postgresMessageTestBotID,
		SessionID: postgresMessageTestSessionID,
		Role:      "assistant",
		Content:   []byte(`{"role":"assistant","content":"old"}`),
	})
	if err != nil {
		t.Fatalf("persist assistant: %v", err)
	}
	oldTurn, err := svc.GetVisibleTurnByMessage(ctx, postgresMessageTestSessionID, assistant.ID)
	if err != nil {
		t.Fatalf("get old turn: %v", err)
	}
	replacement, err := svc.Persist(ctx, PersistInput{
		BotID:           postgresMessageTestBotID,
		SessionID:       postgresMessageTestSessionID,
		Role:            "assistant",
		Content:         []byte(`{"role":"assistant","content":"new"}`),
		SkipHistoryTurn: true,
	})
	if err != nil {
		t.Fatalf("persist replacement assistant: %v", err)
	}
	replacementTurnID, replacementTurnPosition := postgresReplacementIdentity(oldTurn)
	if _, err := svc.ReplaceTurn(ctx, postgresMessageTestSessionID, oldTurn.ID, replacementTurnID, replacementTurnPosition, user.ID, replacement.ID, "retry"); err != nil {
		t.Fatalf("replace turn: %v", err)
	}

	assertPostgresVisibleMessageIDs(t, ctx, svc, user.ID, replacement.ID)
	assertPostgresMessageVisibility(t, ctx, tx, assistant.ID, false, true)
}

// A retry replaces the assistant of the turn and keeps its request. The
// session epoch stays put, so a summary ending before the replaced assistant
// survives, and a summary that reaches it is invalidated.
func TestPostgresReplaceTurnInvalidatesOnlyArtifactsPastTheCutoff(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)

	svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))
	user, err := svc.Persist(ctx, PersistInput{
		BotID:     postgresMessageTestBotID,
		SessionID: postgresMessageTestSessionID,
		Role:      "user",
		Content:   []byte(`{"role":"user","content":"hello"}`),
	})
	if err != nil {
		t.Fatalf("persist user: %v", err)
	}
	assistant, err := svc.Persist(ctx, PersistInput{
		BotID:     postgresMessageTestBotID,
		SessionID: postgresMessageTestSessionID,
		Role:      "assistant",
		Content:   []byte(`{"role":"assistant","content":"old"}`),
	})
	if err != nil {
		t.Fatalf("persist assistant: %v", err)
	}
	oldTurn, err := svc.GetVisibleTurnByMessage(ctx, postgresMessageTestSessionID, assistant.ID)
	if err != nil {
		t.Fatalf("get old turn: %v", err)
	}
	var cutoffMs int64
	if err := tx.QueryRow(ctx, `
		SELECT FLOOR(EXTRACT(EPOCH FROM created_at) * 1000)::bigint FROM bot_history_messages WHERE id = $1
	`, assistant.ID).Scan(&cutoffMs); err != nil {
		t.Fatalf("read assistant created_at: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO bot_history_message_compacts (id, bot_id, session_id, status, summary, anchor_start_ms, anchor_end_ms)
		VALUES
			('00000000-0000-0000-0000-000000074131', $1, $2, 'ok', 'before the turn', 1, $3::bigint - 1),
			('00000000-0000-0000-0000-000000074132', $1, $2, 'ok', 'reaches the turn', 1, $3)
	`, postgresMessageTestBotID, postgresMessageTestSessionID, cutoffMs); err != nil {
		t.Fatalf("insert compaction artifacts: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE bot_history_messages SET compact_id = CASE id
			WHEN $1::uuid THEN '00000000-0000-0000-0000-000000074131'::uuid
			WHEN $2::uuid THEN '00000000-0000-0000-0000-000000074132'::uuid
		END
		WHERE id IN ($1::uuid, $2::uuid)
	`, user.ID, assistant.ID); err != nil {
		t.Fatalf("link rows to compaction artifacts: %v", err)
	}

	replacement, err := svc.Persist(ctx, PersistInput{
		BotID:           postgresMessageTestBotID,
		SessionID:       postgresMessageTestSessionID,
		Role:            "assistant",
		Content:         []byte(`{"role":"assistant","content":"new"}`),
		SkipHistoryTurn: true,
	})
	if err != nil {
		t.Fatalf("persist replacement assistant: %v", err)
	}
	replacementTurnID, replacementTurnPosition := postgresReplacementIdentity(oldTurn)
	if _, err := svc.ReplaceTurn(ctx, postgresMessageTestSessionID, oldTurn.ID, replacementTurnID, replacementTurnPosition, user.ID, replacement.ID, "retry"); err != nil {
		t.Fatalf("replace turn: %v", err)
	}

	var sessionEpoch int64
	if err := tx.QueryRow(ctx, `SELECT compaction_epoch FROM bot_sessions WHERE id = $1`, postgresMessageTestSessionID).Scan(&sessionEpoch); err != nil {
		t.Fatalf("read session compaction epoch: %v", err)
	}
	if sessionEpoch != 0 {
		t.Fatalf("session compaction epoch = %d, want 0: replacement must not advance the epoch", sessionEpoch)
	}
	assertPostgresArtifactInvalidated(t, ctx, tx, "00000000-0000-0000-0000-000000074131", false)
	assertPostgresArtifactInvalidated(t, ctx, tx, "00000000-0000-0000-0000-000000074132", true)
}

// The cutoff rule has a Go half (compaction.HistoryCutoff) and a SQL half
// (ReplaceHistoryTurn). Both run over the same table, so a drift between them
// fails here.
func TestPostgresReplaceTurnInvalidationMatchesCutoffAdmission(t *testing.T) {
	tests := []struct {
		name        string
		status      string
		start       func(cutoffMs int64) int64
		end         func(cutoffMs int64) int64
		invalidated bool
	}{
		{"ends before the cutoff", "ok", constMs(1), offsetMs(-1), false},
		{"ends at the cutoff", "ok", constMs(1), offsetMs(0), true},
		{"ends after the cutoff", "ok", constMs(1), offsetMs(5), true},
		{"no anchor start", "ok", constMs(0), offsetMs(-1), true},
		{"inverted range", "ok", offsetMs(5), offsetMs(-1), true},
		{"pending claim", "pending", constMs(0), constMs(0), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			tx := beginPostgresMessageTestTx(t, ctx)
			setupPostgresMessageTestFixtures(t, ctx, tx)

			svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))
			user, err := svc.Persist(ctx, PersistInput{
				BotID:     postgresMessageTestBotID,
				SessionID: postgresMessageTestSessionID,
				Role:      "user",
				Content:   []byte(`{"role":"user","content":"hello"}`),
			})
			if err != nil {
				t.Fatalf("persist user: %v", err)
			}
			assistant, err := svc.Persist(ctx, PersistInput{
				BotID:     postgresMessageTestBotID,
				SessionID: postgresMessageTestSessionID,
				Role:      "assistant",
				Content:   []byte(`{"role":"assistant","content":"old"}`),
			})
			if err != nil {
				t.Fatalf("persist assistant: %v", err)
			}
			oldTurn, err := svc.GetVisibleTurnByMessage(ctx, postgresMessageTestSessionID, assistant.ID)
			if err != nil {
				t.Fatalf("get old turn: %v", err)
			}
			var cutoffMs int64
			if err := tx.QueryRow(ctx, `
				SELECT FLOOR(EXTRACT(EPOCH FROM created_at) * 1000)::bigint FROM bot_history_messages WHERE id = $1
			`, assistant.ID).Scan(&cutoffMs); err != nil {
				t.Fatalf("read assistant created_at: %v", err)
			}
			const artifactID = "00000000-0000-0000-0000-000000074133"
			if _, err := tx.Exec(ctx, `
				INSERT INTO bot_history_message_compacts (id, bot_id, session_id, status, summary, anchor_start_ms, anchor_end_ms)
				VALUES ($1, $2, $3, $4, 'covers the turn', $5, $6)
			`, artifactID, postgresMessageTestBotID, postgresMessageTestSessionID, tc.status, tc.start(cutoffMs), tc.end(cutoffMs)); err != nil {
				t.Fatalf("insert compaction artifact: %v", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE bot_history_messages SET compact_id = $1 WHERE id = $2
			`, artifactID, assistant.ID); err != nil {
				t.Fatalf("link assistant to compaction artifact: %v", err)
			}

			replacement, err := svc.Persist(ctx, PersistInput{
				BotID:           postgresMessageTestBotID,
				SessionID:       postgresMessageTestSessionID,
				Role:            "assistant",
				Content:         []byte(`{"role":"assistant","content":"new"}`),
				SkipHistoryTurn: true,
			})
			if err != nil {
				t.Fatalf("persist replacement assistant: %v", err)
			}
			replacementTurnID, replacementTurnPosition := postgresReplacementIdentity(oldTurn)
			if _, err := svc.ReplaceTurn(ctx, postgresMessageTestSessionID, oldTurn.ID, replacementTurnID, replacementTurnPosition, user.ID, replacement.ID, "retry"); err != nil {
				t.Fatalf("replace turn: %v", err)
			}
			assertPostgresArtifactInvalidated(t, ctx, tx, artifactID, tc.invalidated)
		})
	}
}

func constMs(v int64) func(int64) int64 {
	return func(int64) int64 { return v }
}

func offsetMs(delta int64) func(int64) int64 {
	return func(cutoffMs int64) int64 { return cutoffMs + delta }
}

func assertPostgresArtifactInvalidated(t *testing.T, ctx context.Context, tx pgx.Tx, artifactID string, want bool) {
	t.Helper()
	var invalidated bool
	if err := tx.QueryRow(ctx, `
		SELECT invalidated_at IS NOT NULL FROM bot_history_message_compacts WHERE id = $1
	`, artifactID).Scan(&invalidated); err != nil {
		t.Fatalf("read artifact %s invalidation: %v", artifactID, err)
	}
	if invalidated != want {
		t.Fatalf("artifact %s invalidated = %v, want %v", artifactID, invalidated, want)
	}
}

func TestPostgresReplaceTurnEditHidesSupersededTurn(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)

	svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))
	oldUser, err := svc.Persist(ctx, PersistInput{
		BotID:     postgresMessageTestBotID,
		SessionID: postgresMessageTestSessionID,
		Role:      "user",
		Content:   []byte(`{"role":"user","content":"old prompt"}`),
	})
	if err != nil {
		t.Fatalf("persist old user: %v", err)
	}
	oldAssistant, err := svc.Persist(ctx, PersistInput{
		BotID:     postgresMessageTestBotID,
		SessionID: postgresMessageTestSessionID,
		Role:      "assistant",
		Content:   []byte(`{"role":"assistant","content":"old answer"}`),
	})
	if err != nil {
		t.Fatalf("persist old assistant: %v", err)
	}
	oldTurn, err := svc.GetVisibleTurnByMessage(ctx, postgresMessageTestSessionID, oldAssistant.ID)
	if err != nil {
		t.Fatalf("get old turn: %v", err)
	}
	newUser, err := svc.Persist(ctx, PersistInput{
		BotID:           postgresMessageTestBotID,
		SessionID:       postgresMessageTestSessionID,
		Role:            "user",
		Content:         []byte(`{"role":"user","content":"new prompt"}`),
		SkipHistoryTurn: true,
	})
	if err != nil {
		t.Fatalf("persist new user: %v", err)
	}
	newAssistant, err := svc.Persist(ctx, PersistInput{
		BotID:           postgresMessageTestBotID,
		SessionID:       postgresMessageTestSessionID,
		Role:            "assistant",
		Content:         []byte(`{"role":"assistant","content":"new answer"}`),
		SkipHistoryTurn: true,
	})
	if err != nil {
		t.Fatalf("persist new assistant: %v", err)
	}
	replacementTurnID, replacementTurnPosition := postgresReplacementIdentity(oldTurn)
	if _, err := svc.ReplaceTurn(ctx, postgresMessageTestSessionID, oldTurn.ID, replacementTurnID, replacementTurnPosition, newUser.ID, newAssistant.ID, "edit"); err != nil {
		t.Fatalf("replace turn: %v", err)
	}

	assertPostgresVisibleMessageIDs(t, ctx, svc, newUser.ID, newAssistant.ID)
	assertPostgresMessageVisibility(t, ctx, tx, oldUser.ID, false, true)
	assertPostgresMessageVisibility(t, ctx, tx, oldAssistant.ID, false, true)
}

func postgresReplacementIdentity(oldTurn HistoryTurn) (string, *int64) {
	position := oldTurn.Position + 1
	return uuid.NewString(), &position
}

func TestPostgresRepairSupersededMessageVisibilityMigration(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)

	if _, err := tx.Exec(ctx, `
		INSERT INTO bot_history_messages (
			id, bot_id, session_id, role, content,
			turn_id, turn_position, turn_message_seq, turn_visible, turn_superseded_at
		)
		VALUES
			(
				'00000000-0000-0000-0000-000000074111',
				$1, $2, 'assistant', '{"role":"assistant","content":"bad"}'::jsonb,
				'00000000-0000-0000-0000-000000074121', 1, 2, true, now()
			),
			(
				'00000000-0000-0000-0000-000000074112',
				$1, $2, 'assistant', '{"role":"assistant","content":"good"}'::jsonb,
				'00000000-0000-0000-0000-000000074122', 2, 2, true, NULL
			)
	`, postgresMessageTestBotID, postgresMessageTestSessionID); err != nil {
		t.Fatalf("insert repair fixtures: %v", err)
	}

	sql, err := os.ReadFile("../../../db/postgres/migrations/0105_repair_superseded_message_visibility.up.sql")
	if err != nil {
		t.Fatalf("read repair migration: %v", err)
	}
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("run repair migration: %v", err)
	}

	assertPostgresMessageVisibility(t, ctx, tx, "00000000-0000-0000-0000-000000074111", false, true)
	assertPostgresMessageVisibility(t, ctx, tx, "00000000-0000-0000-0000-000000074112", true, false)
}

const (
	postgresMessageTestUserID    = "00000000-0000-0000-0000-000000074101"
	postgresMessageTestBotID     = "00000000-0000-0000-0000-000000074102"
	postgresMessageTestSessionID = "00000000-0000-0000-0000-000000074103"
)

func beginPostgresMessageTestTx(t *testing.T, ctx context.Context) pgx.Tx {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("skip postgres integration test: TEST_POSTGRES_DSN is not set")
	}
	pool, err := dbpkg.OpenPostgresDSN(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to configured postgres integration database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping configured postgres integration database: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	return tx
}

func setupPostgresMessageTestFixtures(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	name := fmt.Sprintf("postgres-message-test-%d", time.Now().UnixNano())
	if _, err := tx.Exec(ctx, `
		WITH created_user AS (
			INSERT INTO users (id, username, is_active)
			VALUES ($1, $2, true) RETURNING id
		)
		INSERT INTO team_members (user_id, role)
		SELECT id, 'admin' FROM created_user
	`, postgresMessageTestUserID, name); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO bots (id, owner_user_id, name)
		VALUES ($1, $2, $3)
	`, postgresMessageTestBotID, postgresMessageTestUserID, name); err != nil {
		t.Fatalf("insert bot: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO bot_sessions (id, bot_id, channel_type)
		VALUES ($1, $2, 'local')
	`, postgresMessageTestSessionID, postgresMessageTestBotID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
}

func assertPostgresVisibleMessageIDs(t *testing.T, ctx context.Context, svc *DBService, want ...string) {
	t.Helper()
	messages, err := svc.ListBySession(ctx, postgresMessageTestSessionID)
	if err != nil {
		t.Fatalf("list session messages: %v", err)
	}
	got := make([]string, 0, len(messages))
	for _, msg := range messages {
		got = append(got, msg.ID)
	}
	if len(got) != len(want) {
		t.Fatalf("visible message ids = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("visible message ids = %#v, want %#v", got, want)
		}
	}
}

func assertPostgresMessageVisibility(t *testing.T, ctx context.Context, tx pgx.Tx, messageID string, wantVisible bool, wantSuperseded bool) {
	t.Helper()
	pgMessageID, err := dbpkg.ParseUUID(messageID)
	if err != nil {
		t.Fatalf("parse message id: %v", err)
	}
	var visible bool
	var superseded bool
	if err := tx.QueryRow(ctx, `
		SELECT turn_visible, turn_superseded_at IS NOT NULL
		FROM bot_history_messages
		WHERE id = $1
	`, pgMessageID).Scan(&visible, &superseded); err != nil {
		t.Fatalf("read message visibility: %v", err)
	}
	if visible != wantVisible || superseded != wantSuperseded {
		t.Fatalf("message %s visible/superseded = %v/%v, want %v/%v", messageID, visible, superseded, wantVisible, wantSuperseded)
	}
}

func TestPostgresPersistsBinaryToolOutputAndFollowingMessage(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)
	svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))
	for _, row := range []PersistInput{
		{BotID: postgresMessageTestBotID, SessionID: postgresMessageTestSessionID, Role: "user", Content: []byte(`{"role":"user","content":"read cache"}`)},
		{BotID: postgresMessageTestBotID, SessionID: postgresMessageTestSessionID, Role: "assistant", Content: []byte(`{"role":"assistant","content":[{"type":"tool-call","toolCallId":"sqlite-read","toolName":"exec","input":{}}]}`)},
	} {
		if _, err := svc.Persist(ctx, row); err != nil {
			t.Fatalf("persist step prefix: %v", err)
		}
	}
	output, err := svc.Persist(ctx, PersistInput{
		BotID: postgresMessageTestBotID, SessionID: postgresMessageTestSessionID, Role: "tool",
		Content:     []byte(`{"role":"tool","content":[{"type":"tool-result","toolCallId":"sqlite-read","result":{"stdout":"SQLite format 3\u0000中文","literal":"\\u0000","size":9007199254740993}}]}`),
		Metadata:    map[string]any{"diagnostic": "binary\x00output"},
		DisplayText: "SQLite\x00preview",
	})
	if err != nil {
		t.Fatalf("persist binary tool result: %v", err)
	}
	following, err := svc.Persist(ctx, PersistInput{BotID: postgresMessageTestBotID, SessionID: postgresMessageTestSessionID, Role: "assistant", Content: []byte(`{"role":"assistant","content":"continued"}`)})
	if err != nil {
		t.Fatalf("persist following step: %v", err)
	}
	var stdout, literal, size, metadata, preview string
	err = tx.QueryRow(ctx, `SELECT content #>> '{content,0,result,stdout}', content #>> '{content,0,result,literal}', content #>> '{content,0,result,size}', metadata->>'diagnostic', display_text FROM bot_history_messages WHERE id=$1`, output.ID).Scan(&stdout, &literal, &size, &metadata, &preview)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != `SQLite format 3\x00中文` || literal != `\u0000` || size != "9007199254740993" || metadata != `binary\x00output` || preview != `SQLite\x00preview` {
		t.Fatalf("roundtrip changed content: %q %q %q %q %q", stdout, literal, size, metadata, preview)
	}
	if following.ID == "" {
		t.Fatal("following message missing")
	}
}
