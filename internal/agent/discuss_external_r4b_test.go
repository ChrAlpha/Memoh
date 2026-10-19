package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/pending"
	acpclient "github.com/felinics/memoh/internal/agent/runtime/acp/client"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// r4bDiscussStore adds the reads an External Agent round makes on top of the
// discuss fixture's store, which only knows the reads admission needs.
type r4bDiscussStore struct {
	discussStore
}

func (s r4bDiscussStore) DeleteRuntimeDecisionProjectionsByRun(ctx context.Context, p dbsqlc.DeleteRuntimeDecisionProjectionsByRunParams) (int64, error) {
	return s.pg.DeleteRuntimeDecisionProjectionsByRun(ctx, p)
}

func (s r4bDiscussStore) GetTurnAgentTurnID(ctx context.Context, p dbsqlc.GetTurnAgentTurnIDParams) (string, error) {
	return s.pg.GetTurnAgentTurnID(ctx, p)
}

func (s r4bDiscussStore) InTx(ctx context.Context, fn func(dbstore.Queries) error) error {
	return s.pg.InTx(ctx, fn)
}

func (s r4bDiscussStore) SupportsTransactions() bool { return s.pg.SupportsTransactions() }

// r4bExternalDiscuss is an ACP-backed discuss session: the runtime drives the
// session, so the run is admitted the way the sweep admits one.
func r4bExternalDiscuss(t *testing.T, env discussEnv) (*discussProcess, *recordingACPPrompter) {
	t.Helper()
	pool := &recordingACPPrompter{result: acpclient.PromptResult{Text: "done", StopReason: "end_turn"}}
	proc := newDiscussProcessWith(t, env, []discussOption{
		discussService(withACPPool(pool)),
		discussFixture(withFixtureQueries(func(q dbstore.Queries) dbstore.Queries {
			return r4bDiscussStore{discussStore: q.(discussStore)}
		})),
		discussDeps(func(d *Deps) {
			d.BotPermissions = allowWorkspaceExecForBot(env.botID, env.ownerID)
			d.Sessions = &agenttest.Sessions{
				GetFn: func(_ context.Context, id string) (sessionpkg.Thread, error) {
					return sessionpkg.Thread{
						ID: id, BotID: env.botID, Type: sessionpkg.TypeDiscuss,
						SessionMode: sessionpkg.TypeDiscuss, RuntimeType: sessionpkg.RuntimeACPAgent,
						Metadata: map[string]any{
							"acp_agent_id":             "codex",
							"project_path":             "/data/app",
							"runtime_owner_account_id": env.ownerID,
						},
					}, nil
				},
				UpdateMetadataFn: func(_ context.Context, id string, _ map[string]any) (sessionpkg.Thread, error) {
					return sessionpkg.Thread{ID: id}, nil
				},
			}
		}),
	}, &discussStep{})
	if _, err := env.pool.Exec(context.Background(), `
		UPDATE bot_sessions SET runtime_type = 'acp_agent', runtime_metadata = $2 WHERE id = $1
	`, env.sessionID, `{"runtime_owner_account_id":"`+env.ownerID+`"}`); err != nil {
		t.Fatal(err)
	}
	return proc, pool
}

// An External Agent runtime is reset for the run, so the batch it answers is
// the whole of what it knows: it travels as the prompt, framed by the output
// contract the conversation is answered under. Without the composed prompt the
// driver is handed an empty one, which the runtime refuses outright
// (client.ErrPromptRequired).
func TestR4bExternalDiscussPromptCarriesTheWholeBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	proc, pool := r4bExternalDiscuss(t, env)

	alice := env.participant(t, false)
	env.write(t, passiveInput{from: alice, externalID: "m1", text: "did anyone check the deploy?"})
	env.write(t, passiveInput{from: alice, externalID: "m2", mentionsMe: true, text: "@memoh when is the freeze?"})

	outcome, done := proc.admit(t, env)
	if outcome != pending.DiscussAdmitted {
		t.Fatalf("outcome = %v, want the batch admitted", outcome)
	}
	if done != nil {
		waitDiscussRun(t, done)
	}
	if pool.calls != 1 {
		t.Fatalf("driver prompts = %d, want exactly one", pool.calls)
	}
	prompt := pool.input.Prompt
	if strings.TrimSpace(prompt) == "" {
		t.Fatal("the runtime was prompted with an empty prompt; the batch never left history")
	}
	// Both rows of the batch, including the one that only asked a question.
	for _, want := range []string{"did anyone check the deploy?", "@memoh when is the freeze?"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lost batch input %q:\n%s", want, prompt)
		}
	}
	// The rows keep their order and their framing.
	first, second := strings.Index(prompt, "did anyone check the deploy?"), strings.Index(prompt, "@memoh when is the freeze?")
	if first > second {
		t.Fatalf("the batch reached the runtime out of order:\n%s", prompt)
	}
	if !strings.HasPrefix(prompt, pending.DiscussPromptPrefix) || !strings.HasSuffix(strings.TrimSpace(prompt), pending.DiscussPromptSuffix) {
		t.Fatalf("prompt is not framed by the discuss contract:\n%s", prompt)
	}
	if !strings.Contains(prompt, "[user]") {
		t.Fatalf("prompt lost the row roles:\n%s", prompt)
	}
}

// The batch travels on the request rather than beside it, so the run's resume
// record carries it: a process that dies mid-run replays the batch instead of
// resuming into a run with nothing to answer.
func TestR4bExternalDiscussBatchSurvivesAResumeRecord(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	proc, _ := r4bExternalDiscuss(t, env)

	alice := env.participant(t, false)
	env.write(t, passiveInput{from: alice, externalID: "m1", text: "did anyone check the deploy?"})
	env.write(t, passiveInput{from: alice, externalID: "m2", mentionsMe: true, text: "@memoh when is the freeze?"})

	if outcome, done := proc.admit(t, env); outcome != pending.DiscussAdmitted {
		t.Fatalf("outcome = %v, want the batch admitted", outcome)
	} else if done != nil {
		waitDiscussRun(t, done)
	}

	var recorded string
	if err := env.pool.QueryRow(context.Background(), `
		SELECT COALESCE(input_json->'resume'->>'query', '')
		FROM session_runs
		WHERE session_id = $1 AND input_json ? 'cover_position'
		ORDER BY created_at DESC LIMIT 1`, env.sessionID).Scan(&recorded); err != nil {
		t.Fatalf("read the run's resume record: %v", err)
	}
	if strings.TrimSpace(recorded) == "" {
		t.Fatal("the run recorded no resume context; a crash would resume into an empty run")
	}
	for _, want := range []string{"did anyone check the deploy?", "@memoh when is the freeze?"} {
		if !strings.Contains(recorded, want) {
			t.Fatalf("the resume record lost batch input %q:\n%s", want, recorded)
		}
	}
	if !strings.Contains(recorded, pending.DiscussPromptSuffix) {
		t.Fatalf("the resume record is not the text the run was prompted with:\n%s", recorded)
	}
}

// The batch is the whole context, so a runtime carrying the previous turn over
// would be shown it twice: the run starts from a fresh process.
func TestR4bExternalDiscussAsksForAFreshRuntime(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	proc, pool := r4bExternalDiscuss(t, env)

	alice := env.participant(t, false)
	env.write(t, passiveInput{from: alice, externalID: "m1", mentionsMe: true, text: "@memoh hello"})

	if outcome, done := proc.admit(t, env); outcome != pending.DiscussAdmitted {
		t.Fatalf("outcome = %v, want the batch admitted", outcome)
	} else if done != nil {
		waitDiscussRun(t, done)
	}
	if pool.calls != 1 {
		t.Fatalf("driver prompts = %d, want exactly one", pool.calls)
	}
	if !pool.input.ForceFreshRuntime {
		t.Fatal("the runtime was resumed; the batch would be shown to it twice")
	}
}

// The runtime is prompted as the run's own: the run it belongs to, the route
// the conversation arrives on, the participant who wrote the newest input, and
// the credentials and tool endpoint issued for them.
func TestR4bExternalDiscussRunActsForTheParticipantOnTheRoute(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	t.Setenv("MEMOH_ACP_MCP_HTTP_URL", "http://tools.test/bots/{bot_id}")
	env := newDiscussEnv(t, "group")
	proc, pool := r4bExternalDiscuss(t, env)

	alice := env.participant(t, false)
	hw := env.write(t, passiveInput{from: alice, externalID: "m1", mentionsMe: true, text: "@memoh hello"})
	if outcome, done := proc.admit(t, env); outcome != pending.DiscussAdmitted {
		t.Fatalf("outcome = %v, want the batch admitted", outcome)
	} else if done != nil {
		waitDiscussRun(t, done)
	}
	run := env.waitRunState(t, env.invocation(hw), "completed")

	got := pool.input
	if got.RunID != run.runID {
		t.Fatalf("prompt RunID = %q, want the admitted run %q", got.RunID, run.runID)
	}
	if got.RouteID != env.routeID || got.SessionID != env.sessionID || got.ChannelIdentityID != alice.identityID {
		t.Fatalf("prompt names route %q session %q sender %q, want %q %q %q",
			got.RouteID, got.SessionID, got.ChannelIdentityID, env.routeID, env.sessionID, alice.identityID)
	}
	if got.SessionToken == "" {
		t.Fatal("the runtime was prompted without the credentials issued for the run")
	}
	if want := "http://tools.test/bots/" + env.botID; got.ToolHTTPURL != want {
		t.Fatalf("prompt ToolHTTPURL = %q, want %q", got.ToolHTTPURL, want)
	}
}
