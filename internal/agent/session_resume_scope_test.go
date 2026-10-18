package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/felinics/memoh/internal/agent/admit"
	"github.com/felinics/memoh/internal/agent/resume"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

type (
	resumeScopeKey   struct{}
	testResumeScopes struct {
		teams  []string
		broken bool
	}
)

func (testResumeScopes) CurrentScope(ctx context.Context) string {
	scope, _ := ctx.Value(resumeScopeKey{}).(string)
	return scope
}

func (p testResumeScopes) BindScope(ctx context.Context, scope string) context.Context {
	if p.broken {
		return ctx
	}
	return context.WithValue(ctx, resumeScopeKey{}, scope)
}

func (p testResumeScopes) ListScopePage(_ context.Context, after string, _ int32) (runreq.TeamScopePage, error) {
	// One tenant per page exercises the worker's cross-page cursor.
	for i, scope := range p.teams {
		if after == "" || after == scope {
			index := i
			if after != "" {
				index++
			}
			if index >= len(p.teams) {
				return runreq.TeamScopePage{Complete: true}, nil
			}
			return runreq.TeamScopePage{Scopes: []string{p.teams[index]}, NextCursor: p.teams[index], Complete: index == len(p.teams)-1}, nil
		}
	}
	return runreq.TeamScopePage{}, errors.New("unknown scope cursor")
}

type tenantResumeQueries struct {
	dbstore.Queries
	q    *sqlc.Queries
	seen []string

	mu           sync.Mutex
	sessionReads []string
}

func (q *tenantResumeQueries) ListInterruptedSessionRuns(ctx context.Context, after pgtype.UUID) ([]sqlc.SessionRun, error) {
	scope, _ := ctx.Value(resumeScopeKey{}).(string)
	if scope == "" {
		return nil, errors.New("unbound tenant query")
	}
	q.seen = append(q.seen, scope)
	return q.q.ListInterruptedSessionRuns(ctx, after)
}

func (q *tenantResumeQueries) GetSessionInterruptedRun(ctx context.Context, sessionID pgtype.UUID) (sqlc.SessionRun, error) {
	if scope, _ := ctx.Value(resumeScopeKey{}).(string); scope == "" {
		return sqlc.SessionRun{}, errors.New("unbound tenant query")
	}
	return q.q.GetSessionInterruptedRun(ctx, sessionID)
}

func (q *tenantResumeQueries) RetireSupersededInterruptedSessionRuns(ctx context.Context) (int64, error) {
	if scope, _ := ctx.Value(resumeScopeKey{}).(string); scope == "" {
		return 0, errors.New("unbound tenant retirement")
	}
	return q.q.RetireSupersededInterruptedSessionRuns(ctx)
}

func (q *tenantResumeQueries) RetireInterruptedSessionRun(ctx context.Context, runID pgtype.UUID) (int64, error) {
	if scope, _ := ctx.Value(resumeScopeKey{}).(string); scope == "" {
		return 0, errors.New("unbound tenant retirement")
	}
	return q.q.RetireInterruptedSessionRun(ctx, runID)
}

// GetSessionByID answers the session model-preference read a resumed turn
// makes. The session stores no preference, which is the ordinary case and what
// falls the resolution through to the bot's configured model.
func (q *tenantResumeQueries) GetSessionByID(ctx context.Context, id pgtype.UUID) (sqlc.BotSession, error) {
	scope, _ := ctx.Value(resumeScopeKey{}).(string)
	if scope == "" {
		return sqlc.BotSession{}, errors.New("unbound tenant session read")
	}
	q.mu.Lock()
	q.sessionReads = append(q.sessionReads, scope)
	q.mu.Unlock()
	return q.q.GetSessionByID(ctx, id)
}

// sessionReadScopes is the tenant each session read ran under.
func (q *tenantResumeQueries) sessionReadScopes() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.sessionReads...)
}

func TestResumeScopeBindingFailsClosed(t *testing.T) {
	ctx := t.Context()
	if _, err := runreq.BindTeamScope(ctx, testResumeScopes{broken: true}, uuid.NewString()); !errors.Is(err, runreq.ErrTeamScopeMismatch) {
		t.Fatalf("missing binding=%v", err)
	}
	s := newTestService(t, Deps{}, withTeamScopes(testResumeScopes{}))
	if _, err := startContinuation(context.WithValue(ctx, resumeScopeKey{}, uuid.NewString()), s, sqlc.SessionRun{TeamID: db.ParseUUIDOrEmpty(uuid.NewString())}); !errors.Is(err, runreq.ErrTeamScopeMismatch) {
		t.Fatalf("cross-team row=%v", err)
	}
	if err := runreq.ValidateTeamScopePage(runreq.TeamScopePage{NextCursor: "same"}, "same"); err == nil {
		t.Fatal("non-advancing page accepted")
	}
}

func TestPostgresResumeScopesUseOrdinaryRoleAndPreserveTenantIntoExecution(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is required")
	}
	ctx := t.Context()
	admin, err := db.OpenPostgresDSN(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	teams := []string{uuid.NewString(), uuid.NewString()}
	users := []string{uuid.NewString(), uuid.NewString()}
	sessions := map[string]string{}
	for i, teamID := range teams {
		botID, sessionID := uuid.NewString(), uuid.NewString()
		sessions[teamID] = sessionID
		statements := []struct {
			sql  string
			args []any
		}{
			{"INSERT INTO teams(id) VALUES($1)", []any{teamID}},
			{"INSERT INTO users(id,username,is_active) VALUES($1,$2,true)", []any{users[i], "resume-scope-" + uuid.NewString()}},
			{"INSERT INTO team_members(team_id,user_id,role) VALUES($1,$2,'admin')", []any{teamID, users[i]}},
			{"INSERT INTO bots(team_id,id,name,owner_user_id) VALUES($1,$2,$3,$4)", []any{teamID, botID, "resume-scope-" + uuid.NewString(), users[i]}},
			{"INSERT INTO bot_sessions(team_id,id,bot_id,next_turn_position) VALUES($1,$2,$3,2)", []any{teamID, sessionID, botID}},
		}
		for _, stmt := range statements {
			if _, err := admin.Exec(ctx, stmt.sql, stmt.args...); err != nil {
				t.Fatal(err)
			}
		}
		raw, _ := json.Marshal(map[string]any{"resume": admit.ResumeContext{Version: 1, ChatID: botID, Query: "resume scoped task"}})
		if _, err := admin.Exec(ctx, `INSERT INTO session_runs(team_id,run_id,bot_id,session_id,invocation_id,turn_id,turn_position,state,input_json,input_fingerprint,error_code)
   VALUES($1,$2,$3,$4,'original',$5,1,'lost',$6,'fixture','session_runtime.interrupted')`, teamID, uuid.NewString(), botID, sessionID, uuid.NewString(), raw); err != nil {
			t.Fatal(err)
		}
		defer func() {
			clean := context.WithoutCancel(ctx)
			_, _ = admin.Exec(clean, "DELETE FROM bots WHERE team_id=$1", teamID)
			_, _ = admin.Exec(clean, "DELETE FROM team_members WHERE team_id=$1", teamID)
			_, _ = admin.Exec(clean, "DELETE FROM users WHERE id=$1", users[i])
			_, _ = admin.Exec(clean, "DELETE FROM teams WHERE id=$1", teamID)
		}()
	}
	role := "resume_qa_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	roleSQL := pgx.Identifier{role}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE ROLE "+roleSQL+" NOLOGIN NOSUPERUSER NOBYPASSRLS"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = admin.Exec(context.WithoutCancel(ctx), "DROP OWNED BY "+roleSQL)
		_, _ = admin.Exec(context.WithoutCancel(ctx), "DROP ROLE "+roleSQL)
	}()
	if _, err := admin.Exec(ctx, "GRANT USAGE ON SCHEMA public TO "+roleSQL); err != nil {
		t.Fatal(err)
	}
	// The worker retires superseded intents with the same tenant-bound role.
	if _, err := admin.Exec(ctx, "GRANT SELECT, UPDATE ON session_runs TO "+roleSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "GRANT SELECT ON bot_sessions,bots TO "+roleSQL); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1 // Force tenant changes to reuse the same physical connection.
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error { _, err := c.Exec(ctx, "SET ROLE "+roleSQL); return err }
	cfg.PrepareConn = func(ctx context.Context, c *pgx.Conn) (bool, error) {
		scope, _ := ctx.Value(resumeScopeKey{}).(string)
		if scope == "" {
			return true, errors.New("tenant context required")
		}
		_, err := c.Exec(ctx, "SELECT set_config('memoh.team_id',$1,false)", scope)
		return err == nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	scopes := testResumeScopes{teams: teams}
	bound := scopes.BindScope(ctx, teams[0])
	var super, bypass bool
	if err := pool.QueryRow(bound, "SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&super, &bypass); err != nil || super || bypass {
		t.Fatalf("role flags: %v %v %v", super, bypass, err)
	}
	var visible int
	if err := pool.QueryRow(bound, "SELECT count(*) FROM session_runs").Scan(&visible); err != nil || visible != 1 {
		t.Fatalf("RLS visible=%d err=%v", visible, err)
	}
	q := &tenantResumeQueries{q: sqlc.New(pool)}
	// The resumed turn resolves through the whole application path, and every
	// read it makes goes through the tenant-bound store. Recording the scopes
	// those reads ran under is how the binding is observed: a turn that ran
	// under another tenant's scope would be reading another team's rows.
	s, admitter := newAdmittedTurnTestService(t, withLogger(slog.Default()),
		withDeps(func(d *Deps) { d.Queries = q }), withTeamScopes(scopes))
	scan := resume.Scan{}
	for range 2 {
		if err := s.resume.ScanInterrupted(ctx, &scan); err != nil {
			t.Fatal(err)
		}
	}
	// Each resumed turn reads its session under the team that owns it, so both
	// teams are served and no read ran unbound. The runs continue in their own
	// goroutines, so this waits for them rather than reading a partial set.
	served := map[string]bool{}
	deadline := time.Now().Add(10 * time.Second)
	for len(served) < len(teams) {
		served = map[string]bool{}
		for _, scope := range q.sessionReadScopes() {
			served[scope] = true
		}
		if time.Now().After(deadline) {
			t.Fatalf("resumed turns read under %v, want both teams", served)
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, teamID := range teams {
		if !served[teamID] {
			t.Fatalf("team %s never resumed its session", teamID)
		}
	}
	_ = sessions
	if fmt.Sprint(q.seen) != fmt.Sprint(teams) {
		t.Fatalf("scan scopes=%v", q.seen)
	}
	if len(admitter.Admitted()) != 2 {
		t.Fatal("not all tenant sessions admitted")
	}
	// The unbound startup context cannot access the ordinary pool.
	if _, err := sqlc.New(pool).ListInterruptedSessionRuns(ctx, pgtype.UUID{Valid: true}); err == nil {
		t.Fatal("unbound query succeeded")
	}
}
