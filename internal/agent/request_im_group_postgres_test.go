package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

// An IM group conversation reaches the model as one user row per message,
// each carrying who wrote it, what it answered and whether it addressed the
// bot, with nothing left out for being old and nothing repeated for being
// the message that started the run. The rows are read from PostgreSQL; the
// request is what the model provider receives.
func TestDriveIMGroupRowsFromPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	ctx := context.Background()
	pool := agenttest.OpenPostgres(t, ctx)
	botID, sessionID := agenttest.CreateSessionFixture(t, ctx, pool)
	db := messagepkg.NewService(nil, postgresstore.NewQueriesWithPool(pool, dbsqlc.New(pool)))
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

	persist := func(role, externalID, replyTo, text string, content sdk.Message, rowAt time.Time) {
		t.Helper()
		modelMessage := runreq.SDKMessagesToModelMessages([]sdk.Message{content})[0]
		raw, err := json.Marshal(modelMessage)
		if err != nil {
			t.Fatal(err)
		}
		row, err := db.Persist(ctx, messagepkg.PersistInput{
			BotID: botID, SessionID: sessionID, Role: role, Content: raw,
			SenderChannelIdentityID: "", ExternalMessageID: externalID, SourceReplyToMessageID: replyTo,
			DisplayText: text,
		})
		if err != nil {
			t.Fatalf("persist %s: %v", externalID, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE bot_history_messages SET created_at = $2 WHERE id = $1`, row.ID, rowAt); err != nil {
			t.Fatal(err)
		}
	}
	// A message from the day before: no time window drops it.
	persist("user", "m0", "", "kickoff", sdk.UserMessage(imGroupHeader(imAlice, "m0", "kickoff: freeze is Friday", at.Add(-26*time.Hour), nil)), at.Add(-26*time.Hour))
	// Passive: nobody asked the bot anything.
	persist("user", "m1", "", "deploy", sdk.UserMessage(imGroupHeader(imAlice, "m1", "did anyone check the deploy?", at, nil)), at)
	// Passive reply to another member.
	persist("user", "m2", "m1", "passed", sdk.UserMessage(imGroupHeader(imBob, "m2", "I did, it passed", at.Add(time.Minute), func(in *turn.UserMessageHeaderInput) {
		in.ReplyToMessageID, in.ReplyToSender, in.ReplyToPreview = "m1", "Alice", "did anyone check the deploy?"
	})), at.Add(time.Minute))
	// A message that addressed the bot, and the bot's answer to it.
	persist("user", "m3", "", "ask", sdk.UserMessage(imGroupHeader(imCarol, "m3", "@memoh when is the freeze?", at.Add(2*time.Minute), func(in *turn.UserMessageHeaderInput) {
		in.MentionsMe = true
	})), at.Add(2*time.Minute))
	persist("assistant", "", "", "friday", sdk.AssistantMessage("The freeze is Friday."), at.Add(3*time.Minute))

	fixture, recorder := newDriveRequestFixture(t, driveRequestRuntime(t),
		withFixtureMessages(&pgContextMessages{db: db, sessionID: sessionID}))
	handle, err := fixture.service.StartTurn(ctx, turn.StartTurnCommand{
		SchemaVersion:           1,
		TeamID:                  "drive-request-team",
		Mode:                    turn.ModeChat,
		BotID:                   agenttest.LifecycleBotID,
		ChatID:                  agenttest.LifecycleBotID,
		ThreadID:                agenttest.LifecycleSessionID,
		Query:                   "@memoh and who signs it off?",
		UserVisibleText:         "@memoh and who signs it off?",
		ExternalMessageID:       "m4",
		SourceReplyToMessageID:  "m3",
		ReplySender:             "Carol",
		ReplyPreview:            "@memoh when is the freeze?",
		MentionsBot:             true,
		SourceChannelIdentityID: imBob.identityID,
		DisplayName:             imBob.name,
		CurrentChannel:          "telegram",
		Channels:                []string{"telegram"},
		ReplyTarget:             "im-group-1",
		ConversationType:        "group",
		ConversationName:        "Release crew",
	})
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	drainTurn(t, handle)
	assertDriveGolden(t, "drive-requests", "im_group_rows", marshalDriveRequests(t, requireDriveRequests(t, recorder)))
}
