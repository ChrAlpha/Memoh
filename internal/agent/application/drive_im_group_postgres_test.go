package application

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

// pgContextMessages reads a session's context from the real PostgreSQL store
// under the session id the fixture's run carries; everything else is the
// fixture's in-memory service.
type pgContextMessages struct {
	recordingMessageService
	db        *messagepkg.DBService
	sessionID string
}

func (m *pgContextMessages) ListActiveAfterCompactionBySessionWithinBytes(ctx context.Context, _ string, maxBytes int64) (messagepkg.SessionHistoryAfterCompaction, error) {
	return m.db.ListActiveAfterCompactionBySessionWithinBytes(ctx, m.sessionID, maxBytes)
}

func (m *pgContextMessages) ListRecentUserBySession(ctx context.Context, _ string, limit int) ([]messagepkg.Message, error) {
	return m.db.ListRecentUserBySession(ctx, m.sessionID, limit)
}

type imGroupMember struct {
	identityID string
	name       string
}

var (
	imAlice = imGroupMember{"identity-alice", "Alice"}
	imBob   = imGroupMember{"identity-bob", "Bob"}
	imCarol = imGroupMember{"identity-carol", "Carol"}
)

// imGroupHeader renders a group member's message the way the inbound path
// writes it, for a message nobody asked the bot to answer and for one that
// started a run.
func imGroupHeader(member imGroupMember, messageID, text string, at time.Time, mutate func(*turn.UserMessageHeaderInput)) string {
	input := turn.UserMessageHeaderInput{
		MessageID:         messageID,
		ChannelIdentityID: member.identityID,
		DisplayName:       member.name,
		Channel:           "telegram",
		ConversationType:  "group",
		ConversationName:  "Release crew",
		Target:            "im-group-1",
		Time:              at,
	}
	if mutate != nil {
		mutate(&input)
	}
	return turn.FormatUserHeader(input, text)
}

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
	pool := openTurnAdmissionPostgres(t, ctx)
	botID, sessionID := createTurnAdmissionFixture(t, ctx, pool)
	db := messagepkg.NewService(nil, postgresstore.NewQueriesWithPool(pool, dbsqlc.New(pool)))
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

	persist := func(role, externalID, replyTo, text string, content sdk.Message, rowAt time.Time) {
		t.Helper()
		modelMessage := sdkMessagesToModelMessages([]sdk.Message{content})[0]
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

	fixture, recorder := newDriveRequestFixture(t)
	withDriveRequestRuntime(t, fixture)
	fixture.service.messageService = &pgContextMessages{db: db, sessionID: sessionID}
	handle, err := fixture.service.StartTurn(ctx, turn.StartTurnCommand{
		SchemaVersion:           1,
		TeamID:                  "drive-request-team",
		Mode:                    turn.ModeChat,
		BotID:                   lifecycleTestBotID,
		ChatID:                  lifecycleTestBotID,
		ThreadID:                lifecycleTestSessionID,
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
	drainDriveHandle(t, handle)
	assertDriveGolden(t, "drive-requests", "im_group_rows", marshalDriveRequests(t, requireDriveRequests(t, recorder)))
}
