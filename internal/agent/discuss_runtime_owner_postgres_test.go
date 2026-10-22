package agent

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"

	"github.com/felinics/memoh/internal/agent/internal/pending"
	"github.com/felinics/memoh/internal/auth"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

// bearerSubject is the account a session bearer was issued for.
func bearerSubject(t *testing.T, bearer string) string {
	t.Helper()
	claims := jwt.MapClaims{}
	if _, err := jwt.ParseWithClaims(strings.TrimPrefix(bearer, "Bearer "), claims, func(*jwt.Token) (any, error) {
		return []byte(discussTestSecret), nil
	}); err != nil {
		t.Fatalf("parse the session bearer %q: %v", bearer, err)
	}
	subject, _ := claims["sub"].(string)
	return subject
}

// clearSessionHistory clears a session's history the way the user's "clear"
// does, through the message store.
func clearSessionHistory(t *testing.T, env discussEnv) {
	t.Helper()
	store := messagepkg.NewService(slog.New(slog.DiscardHandler), postgresstore.NewQueriesWithPool(env.pool, dbsqlc.New(env.pool)))
	if err := store.DeleteBySession(context.Background(), env.sessionID); err != nil {
		t.Fatalf("clear session history: %v", err)
	}
}

// storedRuntimeOwner is the session's owner column, empty when it is NULL.
func storedRuntimeOwner(t *testing.T, env discussEnv) string {
	t.Helper()
	var owner *string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT runtime_owner_user_id::text FROM bot_sessions WHERE id = $1`, env.sessionID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner == nil {
		return ""
	}
	return *owner
}

// giveSessionRuntimeOwner records the owner the way creating the session does:
// in the owner column and in both metadata keys. withColumn false leaves the
// column empty, as a session created before it existed.
func giveSessionRuntimeOwner(t *testing.T, env discussEnv, owner string, withColumn bool) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(), `
		UPDATE bot_sessions
		SET metadata = metadata || jsonb_build_object('runtime_owner_account_id', $2::text),
		    runtime_metadata = runtime_metadata || jsonb_build_object('runtime_owner_account_id', $2::text),
		    runtime_owner_user_id = CASE WHEN $3 THEN $2::uuid ELSE NULL END
		WHERE id = $1
	`, env.sessionID, owner, withColumn); err != nil {
		t.Fatal(err)
	}
}

// answerAddressedMessage writes a message that addresses the bot, from a
// participant bound to an account other than owner, and runs the discuss
// admission it wakes. It returns the credentials the run was prompted with.
func answerAddressedMessage(t *testing.T, env discussEnv, proc *discussProcess, pool *recordingACPPrompter, owner string) (sessionBearer, sender string) {
	t.Helper()
	participant := env.participant(t, true)
	if participant.userID == owner {
		t.Fatal("the participant must be someone other than the runtime owner")
	}
	env.write(t, passiveInput{from: participant, externalID: "m1", mentionsMe: true, text: "@memoh hello"})
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
	return pool.input.SessionToken, participant.userID
}

// An External Agent session acts for its runtime owner. Clearing the session's
// history does not change who that is, so the run that answers the next
// addressed message is issued credentials for the owner, not for the
// participant who wrote it.
func TestExternalDiscussAfterClearingHistoryStillActsForTheRuntimeOwner(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	proc, pool := newExternalDiscuss(t, env)
	giveSessionRuntimeOwner(t, env, env.ownerID, true)

	clearSessionHistory(t, env)

	bearer, sender := answerAddressedMessage(t, env, proc, pool, env.ownerID)
	if got := bearerSubject(t, bearer); got != env.ownerID {
		t.Fatalf("the run was issued credentials for %q, want the runtime owner %q (the sender is %q)", got, env.ownerID, sender)
	}
}

// A session created before the owner column has no owner there. The first
// admission that needs it finds the owner in the session's metadata and writes
// it to the column, once; clearing the history afterwards cannot lose it.
func TestExternalDiscussAdoptsTheOwnerOfASessionThatPredatesTheColumn(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	proc, pool := newExternalDiscuss(t, env)
	giveSessionRuntimeOwner(t, env, env.ownerID, false)
	clearSessionHistory(t, env)
	if got := storedRuntimeOwner(t, env); got != "" {
		t.Fatalf("owner column before the first admission = %q, want empty", got)
	}

	bearer, sender := answerAddressedMessage(t, env, proc, pool, env.ownerID)
	if got := bearerSubject(t, bearer); got != env.ownerID {
		t.Fatalf("the run was issued credentials for %q, want the runtime owner %q (the sender is %q)", got, env.ownerID, sender)
	}
	if got := storedRuntimeOwner(t, env); got != env.ownerID {
		t.Fatalf("owner column after the admission = %q, want the owner %q written back", got, env.ownerID)
	}
	if _, err := dbsqlc.New(env.pool).AdoptSessionRuntimeOwnerFromMetadata(context.Background(), db.ParseUUIDOrEmpty(env.sessionID)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("adopting again = %v, want nothing left to write", err)
	}
}

// An owner who left the team leaves the session without one: the column is
// emptied, the metadata names an account that cannot own it, and the admission
// issues no credentials rather than naming the participant.
func TestExternalDiscussWithoutARuntimeOwnerIssuesNoCredentials(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	proc, _ := newExternalDiscuss(t, env)
	leaver := env.participant(t, true)
	giveSessionRuntimeOwner(t, env, leaver.userID, true)
	if got := storedRuntimeOwner(t, env); got != leaver.userID {
		t.Fatalf("owner column = %q, want %q", got, leaver.userID)
	}
	if _, err := env.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, leaver.userID); err != nil {
		t.Fatalf("remove the owner: %v", err)
	}
	if got := storedRuntimeOwner(t, env); got != "" {
		t.Fatalf("owner column after the owner was removed = %q, want it emptied", got)
	}

	sender := env.participant(t, true)
	env.write(t, passiveInput{from: sender, externalID: "m1", mentionsMe: true, text: "@memoh anyone there?"})
	outcome, _, err := proc.svc.pending.AdmitDiscussRun(context.Background(), env.botID, env.sessionID)
	if !errors.Is(err, auth.ErrRuntimeOwnerMissing) {
		t.Fatalf("admission = %v, %v; want it refused for lack of a runtime owner", outcome, err)
	}
}

// The background pass fills the owner column from metadata only where the
// metadata names a member of the team, leaves a column that is already set
// alone, and has nothing to do the second time.
func TestAdoptRuntimeOwnersFromMetadataFillsOnlyWhatItCanOwn(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	ctx := context.Background()
	env := newDiscussEnv(t, "group")
	other := env.participant(t, true)
	stranger := "00000000-0000-0000-0000-0000000000ff"

	sessionWith := func(owner string, column *string) string {
		t.Helper()
		var id string
		if err := env.pool.QueryRow(ctx, `
			INSERT INTO bot_sessions (bot_id, channel_type, runtime_type, metadata, runtime_owner_user_id)
			VALUES ($1, 'telegram', 'codex', jsonb_build_object('runtime_owner_account_id', $2::text), $3::uuid)
			RETURNING id::text
		`, env.botID, owner, column).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	owner := func(id string) string {
		t.Helper()
		var got *string
		if err := env.pool.QueryRow(ctx, `SELECT runtime_owner_user_id::text FROM bot_sessions WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got == nil {
			return ""
		}
		return *got
	}
	member := sessionWith(env.ownerID, nil)
	notAMember := sessionWith(stranger, nil)
	notAnAccount := sessionWith("not-a-uuid", nil)
	alreadySet := sessionWith(env.ownerID, &other.userID)

	queries := dbsqlc.New(env.pool)
	if _, err := queries.AdoptSessionRuntimeOwnersFromMetadata(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	if got := owner(member); got != env.ownerID {
		t.Fatalf("member's session owner = %q, want %q", got, env.ownerID)
	}
	if got := owner(notAMember); got != "" {
		t.Fatalf("session naming a stranger got owner %q, want none", got)
	}
	if got := owner(notAnAccount); got != "" {
		t.Fatalf("session naming no account got owner %q, want none", got)
	}
	if got := owner(alreadySet); got != other.userID {
		t.Fatalf("a column already set became %q, want it left at %q", got, other.userID)
	}
	if again, err := queries.AdoptSessionRuntimeOwnersFromMetadata(ctx, 1000); err != nil || again != 0 {
		t.Fatalf("second pass filled %d, %v; want nothing", again, err)
	}
}
