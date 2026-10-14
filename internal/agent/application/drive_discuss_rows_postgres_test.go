package application

import (
	"bytes"
	"regexp"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/turn"
)

var (
	driveDiscussBotName = regexp.MustCompile(`turn-admission-[0-9a-f-]{36}`)
	driveDiscussUUID    = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
)

// A discuss run starts from the rows of an IM group conversation, one user row
// for each message, each carrying who wrote it, what it answered and whether it
// addressed the bot. The run is admitted for the newest of them, and the model
// is shown all of them, the older ones as history, and told it speaks only
// through send. The rows are read from PostgreSQL; the request is what the
// model provider receives, with the identifiers the database drew replaced by
// fixed ones.
func TestDriveDiscussRowsFromPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	proc := newDiscussProcess(t, env, &discussStep{})
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	alice, bob, carol := env.participant(t, false), env.participant(t, false), env.participant(t, false)

	// A message from the day before: no time window drops it.
	env.write(t, passiveInput{from: alice, externalID: "m0", text: imGroupHeader(imAlice, "m0", "kickoff: freeze is Friday", at.Add(-26*time.Hour), nil)})
	env.write(t, passiveInput{from: alice, externalID: "m1", text: imGroupHeader(imAlice, "m1", "did anyone check the deploy?", at, nil)})
	env.write(t, passiveInput{from: bob, externalID: "m2", text: imGroupHeader(imBob, "m2", "I did, it passed", at.Add(time.Minute), func(in *turn.UserMessageHeaderInput) {
		in.ReplyToMessageID, in.ReplyToSender, in.ReplyToPreview = "m1", "Alice", "did anyone check the deploy?"
	})})
	hw := env.write(t, passiveInput{from: carol, externalID: "m3", mentionsMe: true, text: imGroupHeader(imCarol, "m3", "@memoh when is the freeze?", at.Add(2*time.Minute), func(in *turn.UserMessageHeaderInput) {
		in.MentionsMe = true
	})})

	outcome, done := proc.admit(t, env)
	if outcome != DiscussAdmitted {
		t.Fatalf("outcome = %v, want admitted", outcome)
	}
	waitDiscussRun(t, done)
	env.waitRunState(t, env.invocation(hw), "completed")

	requests := proc.provider.recorded()
	if len(requests) == 0 {
		t.Fatal("the run did not reach the model provider")
	}
	got := marshalDriveRequests(t, requests[:1])
	got = driveDiscussBotName.ReplaceAll(got, []byte("<bot-name>"))
	got = bytes.ReplaceAll(got, []byte(env.botID), []byte("<bot-id>"))
	got = bytes.ReplaceAll(got, []byte(env.sessionID), []byte("<session-id>"))
	got = bytes.ReplaceAll(got, []byte(env.routeID), []byte("<route-id>"))
	got = bytes.ReplaceAll(got, []byte(env.ownerID), []byte("<owner-id>"))
	if leftover := driveDiscussUUID.Find(got); leftover != nil {
		t.Fatalf("the request carries an identifier the database drew: %s", leftover)
	}
	assertDriveGolden(t, "drive-requests", "discuss_rows", got)
}
