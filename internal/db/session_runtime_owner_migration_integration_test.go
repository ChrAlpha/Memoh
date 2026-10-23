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

	"github.com/felinics/memoh/internal/team"
)

// 0181 fills the owner column once from the key in metadata. It runs as the
// table owner without BYPASSRLS and without a tenant context, so the fill has
// to get past FORCE RLS on bot_sessions and team_members, and it must leave
// both policies in force afterwards.
func TestSessionRuntimeOwnerMigrationFillsFromMetadata(t *testing.T) {
	ctx := context.Background()
	dsn := teamMigrationDSN(t)
	pool := freshMigratedDB(t)

	stepDown(t, dsn, countMigrationsFrom(t, "0181_session_runtime_owner.up.sql"))
	sessions := seedRuntimeOwnerFixture(t, ctx, pool)

	role := fmt.Sprintf("runtime_owner_migration_%d_%d", os.Getpid(), teamTestDBSeq.Add(1))
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
		"ALTER TABLE public.team_members OWNER TO " + quotedRole,
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

	// Control: without the migration's own RLS handling this role cannot touch
	// the table, so a filled column below is the migration's doing.
	if _, err := conn.Exec(ctx, "UPDATE public.bot_sessions SET updated_at = updated_at"); sqlState(err) != "42501" {
		t.Fatalf("bare update as the migration role: got %v, want SQLSTATE 42501 (memoh.team_id is not set)", err)
	}

	up, err := fs.ReadFile(postgresMigrationsFS(t), "0181_session_runtime_owner.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := conn.Exec(ctx, string(up)); err != nil {
			t.Fatalf("0181 up: %v", err)
		}
		filled := 0
		for name, s := range sessions {
			var got pgtype.UUID
			mustScan(t, pool.QueryRow(ctx, `SELECT runtime_owner_user_id FROM bot_sessions WHERE id = $1`, s.id), &got)
			if got.Valid != (s.owner != "") || (got.Valid && got.String() != s.owner) {
				t.Fatalf("%s: runtime_owner_user_id = %v, want %q", name, got, s.owner)
			}
			if got.Valid {
				filled++
			}
		}
		var total int
		mustScan(t, pool.QueryRow(ctx, `SELECT count(*) FROM bot_sessions WHERE runtime_owner_user_id IS NOT NULL`), &total)
		if filled != 2 || total != 2 {
			t.Fatalf("filled sessions = %d (column holds %d), want exactly the 2 sessions whose owner is a team member", filled, total)
		}
		for _, table := range []string{"public.bot_sessions", "public.team_members"} {
			var enabled, forced bool
			mustScan(t, pool.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = $1::regclass`, table), &enabled, &forced)
			if !enabled || !forced {
				t.Fatalf("%s RLS enabled=%t forced=%t after 0181", table, enabled, forced)
			}
		}
		// The fill writes the new column only: the JSON copies stay as they were.
		var keptMetadata, keptRuntimeMetadata string
		mustScan(t, pool.QueryRow(ctx, `SELECT metadata->>'runtime_owner_account_id', runtime_metadata->>'runtime_owner_account_id' FROM bot_sessions WHERE id = $1`,
			sessions["direct runtime, owner is a member"].id), &keptMetadata, &keptRuntimeMetadata)
		if want := sessions["direct runtime, owner is a member"].owner; keptMetadata != want || keptRuntimeMetadata != want {
			t.Fatalf("JSON owner copies = %q / %q, want both left at %q", keptMetadata, keptRuntimeMetadata, want)
		}
	}
}

type runtimeOwnerFixture struct {
	id    pgtype.UUID
	owner string
}

func seedRuntimeOwnerFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]runtimeOwnerFixture {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	useTeam := func(id string) {
		if _, err := tx.Exec(ctx, "SELECT set_config('memoh.team_id', $1, true)", id); err != nil {
			t.Fatal(err)
		}
	}
	const otherTeam = "00000000-0000-0000-0000-0000000000b2"
	if _, err := tx.Exec(ctx, `INSERT INTO teams (id, slug) VALUES ($1, 'runtime-owner-other')`, otherTeam); err != nil {
		t.Fatal(err)
	}
	user := func(name string) (id pgtype.UUID) {
		mustScan(t, tx.QueryRow(ctx, `INSERT INTO users (username) VALUES ($1) RETURNING id`, name), &id)
		return id
	}
	member := user("owner-member")
	secondMember := user("owner-second-member")
	nonMember := user("owner-non-member")
	otherMember := user("owner-other-team-member")

	useTeam(team.DefaultTeamID)
	for _, u := range []pgtype.UUID{member, secondMember} {
		if _, err := tx.Exec(ctx, `INSERT INTO team_members (team_id, user_id) VALUES ($1, $2)`, team.DefaultTeamID, u); err != nil {
			t.Fatal(err)
		}
	}
	var bot pgtype.UUID
	mustScan(t, tx.QueryRow(ctx, `INSERT INTO bots (team_id, owner_user_id, name) VALUES ($1, $2, 'owner-bot') RETURNING id`, team.DefaultTeamID, member), &bot)

	session := func(teamID string, botID pgtype.UUID, typ, runtime, metadata, runtimeMetadata string) (id pgtype.UUID) {
		mustScan(t, tx.QueryRow(ctx, `
			INSERT INTO bot_sessions (team_id, bot_id, type, session_mode, runtime_type, metadata, runtime_metadata)
			VALUES ($1, $2, $3, 'chat', $4, $5::jsonb, $6::jsonb) RETURNING id`,
			teamID, botID, typ, runtime, metadata, runtimeMetadata), &id)
		return id
	}
	owned := func(u pgtype.UUID) string { return fmt.Sprintf(`{"runtime_owner_account_id": %q}`, u.String()) }

	out := map[string]runtimeOwnerFixture{
		"direct runtime, owner is a member": {id: session(team.DefaultTeamID, bot, "chat", "codex", owned(member), owned(member)), owner: member.String()},
		"acp runtime, owner is a member":    {id: session(team.DefaultTeamID, bot, "acp_agent", "acp_agent", owned(secondMember), "{}"), owner: secondMember.String()},
		"owner is not a team member":        {id: session(team.DefaultTeamID, bot, "chat", "codex", owned(nonMember), owned(nonMember))},
		"malformed owner":                   {id: session(team.DefaultTeamID, bot, "chat", "codex", `{"runtime_owner_account_id": "not-a-uuid"}`, "{}")},
		"owner only in runtime_metadata":    {id: session(team.DefaultTeamID, bot, "chat", "codex", "{}", owned(member))},
		"model session":                     {id: session(team.DefaultTeamID, bot, "chat", "model", owned(member), "{}")},
	}

	useTeam(otherTeam)
	if _, err := tx.Exec(ctx, `INSERT INTO team_members (team_id, user_id) VALUES ($1, $2)`, otherTeam, otherMember); err != nil {
		t.Fatal(err)
	}
	var otherBot pgtype.UUID
	mustScan(t, tx.QueryRow(ctx, `INSERT INTO bots (team_id, owner_user_id, name) VALUES ($1, $2, 'other-owner-bot') RETURNING id`, otherTeam, otherMember), &otherBot)
	out["owner is a member of another team only"] = runtimeOwnerFixture{id: session(otherTeam, otherBot, "chat", "codex", owned(member), "{}")}

	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return out
}
