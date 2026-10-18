//go:build integration

package db_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/sessionfacts"
	"github.com/felinics/memoh/internal/team"
)

type sessionFactsRow struct {
	ReplyDelivery, TriggerScope, DecisionMode, Audience, ExternalRuntimeDriver string
}

func presetRow(mode string, audience sessionfacts.Audience) sessionFactsRow {
	p := sessionfacts.Preset(mode)
	return sessionFactsRow{
		ReplyDelivery:         string(p.ReplyDelivery),
		TriggerScope:          string(p.TriggerScope),
		DecisionMode:          string(p.DecisionMode),
		Audience:              string(audience),
		ExternalRuntimeDriver: string(p.ExternalRuntimeDriver),
	}
}

// 0169 records the facts session_mode stood in for. The backfill must give
// every existing session the facts its mode implied, under the FORCE RLS a
// migration connection runs with, and the chain must step over it both ways.
func TestSessionFactsMigrationBackfillsFromMode(t *testing.T) {
	ctx := context.Background()
	dsn := teamMigrationDSN(t)
	pool := freshMigratedDB(t)

	sessions := seedSessionFactsFixture(t, ctx, pool)

	// Run the migration the way production does: as the table owner, without
	// superuser and without a tenant context.
	role := fmt.Sprintf("session_facts_migration_%d_%d", os.Getpid(), teamTestDBSeq.Add(1))
	quotedRole := pgx.Identifier{role}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE ROLE "+quotedRole+" NOLOGIN NOSUPERUSER NOCREATEROLE NOBYPASSRLS"); err != nil {
		t.Fatalf("create migration role: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.WithoutCancel(ctx), "REASSIGN OWNED BY "+quotedRole+" TO "+pgx.Identifier{pool.Config().ConnConfig.User}.Sanitize())
		_, _ = pool.Exec(context.WithoutCancel(ctx), "DROP OWNED BY "+quotedRole)
		_, _ = pool.Exec(context.WithoutCancel(ctx), "DROP ROLE "+quotedRole)
	})
	for _, stmt := range []string{
		"ALTER TABLE public.bot_sessions OWNER TO " + quotedRole,
		"ALTER TABLE public.bot_channel_routes OWNER TO " + quotedRole,
		"GRANT USAGE ON SCHEMA public TO " + quotedRole,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect migration role: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "SET ROLE "+quotedRole); err != nil {
		t.Fatalf("set migration role: %v", err)
	}
	up, err := fs.ReadFile(postgresMigrationsFS(t), "0169_session_facts.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	down, err := fs.ReadFile(postgresMigrationsFS(t), "0169_session_facts.down.sql")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := conn.Exec(ctx, string(down)); err != nil {
		t.Fatalf("0169 down: %v", err)
	}
	assertSessionFactColumns(t, ctx, pool, false)
	for range 2 {
		if _, err := conn.Exec(ctx, string(up)); err != nil {
			t.Fatalf("0169 up: %v", err)
		}
		assertSessionFactColumns(t, ctx, pool, true)
		assertSessionTablesKeepRLS(t, ctx, pool)
		for name, s := range sessions {
			var got sessionFactsRow
			mustScan(t, pool.QueryRow(ctx, `
				SELECT reply_delivery, trigger_scope, decision_mode, audience, external_runtime_driver
				FROM bot_sessions WHERE id = $1`, s.id),
				&got.ReplyDelivery, &got.TriggerScope, &got.DecisionMode, &got.Audience, &got.ExternalRuntimeDriver)
			if got != s.want {
				t.Fatalf("%s: backfilled facts = %+v, want %+v", name, got, s.want)
			}
		}
	}

	// Later migrations sit above 0169, so stepping over it counts from head.
	t.Run("chain steps over 0169 in both directions", func(t *testing.T) {
		steps := countMigrationsFrom(t, "0169_session_facts.up.sql")
		stepDown(t, dsn, steps)
		assertSessionFactColumns(t, ctx, pool, false)
		stepUp(t, dsn, steps)
		assertSessionFactColumns(t, ctx, pool, true)
	})

	t.Run("canonical init contains the fact columns", func(t *testing.T) {
		dsn := teamMigrationDSN(t)
		pool := resetToEmpty(t)
		applyCanonicalInitOnly(t, dsn)
		assertSessionFactColumns(t, ctx, pool, true)
	})
}

// A mode change replaces the facts with the new mode's preset; any other
// descriptor update keeps the recorded facts, and audience never moves.
func TestSessionFactsFollowModeChangesOnly(t *testing.T) {
	ctx := context.Background()
	pool := freshMigratedDB(t)
	sessions := seedSessionFactsFixture(t, ctx, pool)
	sessionID := sessions["chat in group"].id

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('memoh.team_id', $1, true)", team.DefaultTeamID); err != nil {
		t.Fatal(err)
	}
	q := sqlc.New(tx)
	update := func(mode string) sqlc.BotSession {
		p := sessionfacts.Preset(mode)
		row, err := q.UpdateSessionTypeAndMetadata(ctx, sqlc.UpdateSessionTypeAndMetadataParams{
			ID: sessionID, Type: mode, SessionMode: mode, RuntimeType: "model",
			ReplyDelivery: string(p.ReplyDelivery), TriggerScope: string(p.TriggerScope),
			DecisionMode: string(p.DecisionMode), ExternalRuntimeDriver: string(p.ExternalRuntimeDriver),
			RuntimeMetadata: []byte("{}"), Metadata: []byte("{}"),
		})
		if err != nil {
			t.Fatalf("update to %s: %v", mode, err)
		}
		return row
	}

	if _, err := tx.Exec(ctx, "UPDATE bot_sessions SET audience = 'shared' WHERE id = $1", sessionID); err != nil {
		t.Fatal(err)
	}
	row := update("discuss")
	if row.ReplyDelivery != "explicit" || row.TriggerScope != "uncovered_events" || row.DecisionMode != "auto_reject" || row.ExternalRuntimeDriver != "participants" {
		t.Fatalf("chat -> discuss kept chat facts: %+v", row)
	}
	if row.Audience != "shared" {
		t.Fatalf("audience moved with the mode: %q", row.Audience)
	}

	if _, err := tx.Exec(ctx, "UPDATE bot_sessions SET decision_mode = 'park' WHERE id = $1", sessionID); err != nil {
		t.Fatal(err)
	}
	if row := update("discuss"); row.DecisionMode != "park" {
		t.Fatalf("same-mode update overwrote a recorded fact: %+v", row)
	}

	if _, err := tx.Exec(ctx, "UPDATE bot_sessions SET reply_delivery = 'loud' WHERE id = $1", sessionID); sqlState(err) != "23514" {
		t.Fatalf("unknown reply_delivery: got %v, want check violation", err)
	}
}

type sessionFactsFixture struct {
	id   pgtype.UUID
	want sessionFactsRow
}

func seedSessionFactsFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]sessionFactsFixture {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('memoh.team_id', $1, true)", team.DefaultTeamID); err != nil {
		t.Fatal(err)
	}
	var userID, botID pgtype.UUID
	mustScan(t, tx.QueryRow(ctx, `INSERT INTO users (username) VALUES ('facts-owner') RETURNING id`), &userID)
	if _, err := tx.Exec(ctx, `INSERT INTO team_members (team_id, user_id) VALUES ($1, $2)`, team.DefaultTeamID, userID); err != nil {
		t.Fatal(err)
	}
	mustScan(t, tx.QueryRow(ctx, `INSERT INTO bots (team_id, owner_user_id, name) VALUES ($1, $2, 'facts-bot') RETURNING id`, team.DefaultTeamID, userID), &botID)
	route := func(channelType, conversationType string) pgtype.UUID {
		var id pgtype.UUID
		var ct any
		if conversationType != "-" {
			ct = conversationType
		}
		mustScan(t, tx.QueryRow(ctx, `
			INSERT INTO bot_channel_routes (team_id, bot_id, channel_type, external_conversation_id, conversation_type)
			VALUES ($1, $2, $3, gen_random_uuid()::text, $4) RETURNING id`, team.DefaultTeamID, botID, channelType, ct), &id)
		return id
	}
	group := route("telegram", "group")
	thread := route("discord", "thread")
	private := route("telegram", "private")
	direct := route("feishu", "p2p")
	untyped := route("telegram", "-")
	web := route("web", "group")

	session := func(mode, runtime string, routeID pgtype.UUID) pgtype.UUID {
		var id pgtype.UUID
		typ := mode
		if mode == "chat" && runtime == "acp_agent" {
			typ = "acp_agent"
		}
		mustScan(t, tx.QueryRow(ctx, `
			INSERT INTO bot_sessions (team_id, bot_id, route_id, type, session_mode, runtime_type)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, team.DefaultTeamID, botID, routeID, typ, mode, runtime), &id)
		return id
	}
	none := pgtype.UUID{}
	out := map[string]sessionFactsFixture{
		"web chat":             {id: session("chat", "model", none), want: presetRow("chat", sessionfacts.AudiencePersonal)},
		"chat in group":        {id: session("chat", "model", group), want: presetRow("chat", sessionfacts.AudienceShared)},
		"chat in private":      {id: session("chat", "model", private), want: presetRow("chat", sessionfacts.AudiencePersonal)},
		"chat in p2p":          {id: session("chat", "model", direct), want: presetRow("chat", sessionfacts.AudiencePersonal)},
		"chat untyped route":   {id: session("chat", "model", untyped), want: presetRow("chat", sessionfacts.AudiencePersonal)},
		"chat on web route":    {id: session("chat", "model", web), want: presetRow("chat", sessionfacts.AudiencePersonal)},
		"acp chat":             {id: session("chat", "acp_agent", private), want: presetRow("chat", sessionfacts.AudiencePersonal)},
		"discuss in group":     {id: session("discuss", "model", group), want: presetRow("discuss", sessionfacts.AudienceShared)},
		"discuss acp in group": {id: session("discuss", "acp_agent", group), want: presetRow("discuss", sessionfacts.AudienceShared)},
		"discuss in thread":    {id: session("discuss", "codex", thread), want: presetRow("discuss", sessionfacts.AudienceShared)},
		"schedule":             {id: session("schedule", "model", none), want: presetRow("schedule", sessionfacts.AudiencePersonal)},
		"subagent":             {id: session("subagent", "model", none), want: presetRow("subagent", sessionfacts.AudiencePersonal)},
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertSessionFactColumns(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want bool) {
	t.Helper()
	var count int
	mustScan(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'bot_sessions'
		  AND column_name IN ('reply_delivery', 'trigger_scope', 'decision_mode', 'audience', 'external_runtime_driver')
		  AND is_nullable = 'NO'`), &count)
	if (count == 5) != want || (count != 0 && count != 5) {
		t.Fatalf("bot_sessions fact columns = %d, want present=%t", count, want)
	}
}

func assertSessionTablesKeepRLS(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, table := range []string{"public.bot_sessions", "public.bot_channel_routes"} {
		var enabled, forced bool
		mustScan(t, pool.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = $1::regclass`, table), &enabled, &forced)
		if !enabled || !forced {
			t.Fatalf("%s RLS enabled=%t forced=%t after 0169", table, enabled, forced)
		}
	}
}

// migrationsFrom counts the up migrations numbered version or above.
