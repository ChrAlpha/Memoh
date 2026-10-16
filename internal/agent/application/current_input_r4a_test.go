package application

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

func r4aOpenHistory(t *testing.T) (*messagepkg.DBService, *pgxpool.Pool, string, string) {
	t.Helper()
	ctx := context.Background()
	pool := openTurnAdmissionPostgres(t, ctx)
	botID, sessionID := createTurnAdmissionFixture(t, ctx, pool)
	return messagepkg.NewService(nil, postgresstore.NewQueriesWithPool(pool, dbsqlc.New(pool))), pool, botID, sessionID
}

// r4aSender creates a channel identity so a row can carry a real sender, and
// returns its id.
func r4aSender(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO channel_identities (id, channel_type, channel_subject_id, display_name)
		VALUES ($1, 'telegram', $2, $3)
	`, id, "subject-"+id, name); err != nil {
		t.Fatalf("create channel identity %s: %v", name, err)
	}
	return id
}

// r4aPersistRow writes one user row carrying the identity a channel writer
// records, at a chosen point on the fixture's own clock so the load order is
// the test's.
func r4aPersistRow(t *testing.T, ctx context.Context, db *messagepkg.DBService, pool *pgxpool.Pool, botID, sessionID, externalID, senderID, text string, at time.Time) string {
	t.Helper()
	content, err := json.Marshal(sdkMessagesToModelMessages([]sdk.Message{sdk.UserMessage(text)})[0])
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.Persist(ctx, messagepkg.PersistInput{
		BotID: botID, SessionID: sessionID, Role: "user", Content: content,
		SenderChannelIdentityID: senderID, ExternalMessageID: externalID, DisplayText: text,
	})
	if err != nil {
		t.Fatalf("persist %s: %v", externalID, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE bot_history_messages SET created_at = $2 WHERE id = $1`, row.ID, at); err != nil {
		t.Fatal(err)
	}
	return row.ID
}

// r4aAssemble reads one run's context through the shared entry point.
func r4aAssemble(t *testing.T, db *messagepkg.DBService, botID, sessionID string, chat ChatRequest, budget int) assembledContext {
	t.Helper()
	service := &Service{messageService: db, logger: slog.New(slog.DiscardHandler)}
	chat.BotID, chat.ChatID, chat.ThreadID = botID, botID, sessionID
	assembled, err := service.assembleContext(context.Background(), chatContextRequest(contextPurposeRun, chat, budget))
	if err != nil {
		t.Fatalf("assembleContext: %v", err)
	}
	return assembled
}

func r4aFind(records []historyfrag.HistoryRecord, dbMessageID string) *historyfrag.HistoryRecord {
	for i := range records {
		if strings.TrimSpace(records[i].DBMessageID) == dbMessageID {
			return &records[i]
		}
	}
	return nil
}

// The row a run answers under the reuse rule reaches the context by identity
// even when the byte budget kept only the tail of the session: the row is the
// one the request named, not the newest one and not the one whose text happens
// to match.
func TestR4aCurrentRowStaysInContextUnderByteBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	ctx := context.Background()
	db, pool, botID, sessionID := r4aOpenHistory(t)
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	alice, carol := r4aSender(t, ctx, pool, "Alice"), r4aSender(t, ctx, pool, "Carol")

	r4aPersistRow(t, ctx, db, pool, botID, sessionID, "m-old-a", alice, "old-a "+strings.Repeat("padding ", 400), at)
	r4aPersistRow(t, ctx, db, pool, botID, sessionID, "m-old-b", alice, "old-b "+strings.Repeat("padding ", 400), at.Add(time.Minute))
	currentID := r4aPersistRow(t, ctx, db, pool, botID, sessionID, "m-current", carol, "current input", at.Add(2*time.Minute))

	// 400 tokens is 1600 bytes of content, below the two old rows alone.
	assembled := r4aAssemble(t, db, botID, sessionID, ChatRequest{
		RequiredHistoryMessageID:  currentID,
		ReusePersistedUserMessage: true,
	}, 400)

	current := r4aFind(assembled.Records, currentID)
	if current == nil {
		t.Fatalf("the row the request named was dropped by the byte budget: %v", stage4Labels(assembled.Messages))
	}
	if !current.Required {
		t.Fatal("the current row reached the context without being marked required")
	}
	if got := current.ExternalMessageID; got != "m-current" {
		t.Fatalf("current row external message id = %q, want m-current", got)
	}
	if got := current.SenderChannelIdentityID; got != carol {
		t.Fatalf("current row sender = %q, want %q", got, carol)
	}
}

// The row the request names keeps its own identity when later rows exist after
// it: neither a later external message nor the bot's own echo takes over the
// current input's message id or sender.
func TestR4aCurrentRowIdentitySurvivesLaterMessages(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	ctx := context.Background()
	db, pool, botID, sessionID := r4aOpenHistory(t)
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	alice, carol, bob := r4aSender(t, ctx, pool, "Alice"), r4aSender(t, ctx, pool, "Carol"), r4aSender(t, ctx, pool, "Bob")

	r4aPersistRow(t, ctx, db, pool, botID, sessionID, "m-old", alice, "old "+strings.Repeat("padding ", 200), at)
	currentID := r4aPersistRow(t, ctx, db, pool, botID, sessionID, "m-current", carol, "the message the bot answers", at.Add(time.Minute))
	r4aPersistRow(t, ctx, db, pool, botID, sessionID, "m-later", bob, "a newer message from somebody else", at.Add(2*time.Minute))

	assembled := r4aAssemble(t, db, botID, sessionID, ChatRequest{
		RequiredHistoryMessageID:  currentID,
		ReusePersistedUserMessage: true,
	}, 100000)

	current := r4aFind(assembled.Records, currentID)
	if current == nil {
		t.Fatalf("the row the request named is not in the context: %v", stage4Labels(assembled.Messages))
	}
	if got := current.ExternalMessageID; got != "m-current" {
		t.Fatalf("current row external message id = %q, want m-current", got)
	}
	if got := current.SenderChannelIdentityID; got != carol {
		t.Fatalf("current row sender = %q, want %q", got, carol)
	}
	if got := current.Ref.ID; got != currentID {
		t.Fatalf("current row ref = %q, want its own row id %q", got, currentID)
	}
}

// A current input larger than the whole byte budget is still in the context:
// the load admits it as the crossing row, and nothing on this path trims it
// away. The request is left for the provider to reject.
func TestR4aOversizedCurrentRowIsSentNotDropped(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	ctx := context.Background()
	db, pool, botID, sessionID := r4aOpenHistory(t)
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	alice, carol := r4aSender(t, ctx, pool, "Alice"), r4aSender(t, ctx, pool, "Carol")

	r4aPersistRow(t, ctx, db, pool, botID, sessionID, "m-old", alice, "old "+strings.Repeat("padding ", 400), at)
	currentID := r4aPersistRow(t, ctx, db, pool, botID, sessionID, "m-current", carol, "current "+strings.Repeat("oversized ", 5000), at.Add(time.Minute))

	assembled := r4aAssemble(t, db, botID, sessionID, ChatRequest{
		RequiredHistoryMessageID:  currentID,
		ReusePersistedUserMessage: true,
	}, 100)

	current := r4aFind(assembled.Records, currentID)
	if current == nil {
		t.Fatalf("an oversized current input was dropped instead of being sent: %v", stage4Labels(assembled.Messages))
	}
	if !strings.Contains(current.ModelMessage.TextContent(), "oversized") {
		t.Fatal("the current row reached the context without its text")
	}
	if !assembled.Truncated {
		t.Fatal("the load did not report that it stopped before the oldest row")
	}
}

// r4aSdkUserText returns the text of every user message the provider request
// carried.
func r4aSdkUserText(request sdk.Request) []string {
	var texts []string
	for _, message := range request.Messages {
		if message.Role != sdk.MessageRoleUser {
			continue
		}
		for _, part := range message.Content {
			if text, ok := part.(sdk.TextPart); ok {
				texts = append(texts, text.Text)
			}
		}
	}
	return texts
}

// r4aSdkUserImages counts the image parts on the request's user messages.
func r4aSdkUserImages(request sdk.Request) int {
	total := 0
	for _, message := range request.Messages {
		if message.Role != sdk.MessageRoleUser {
			continue
		}
		for _, part := range message.Content {
			if _, ok := part.(sdk.ImagePart); ok {
				total++
			}
		}
	}
	return total
}

// A run that carries its input as its own query still sends it when the stored
// history was truncated to fit the byte budget, and the header names the
// message and its sender. The image the user attached rides the same message.
func TestR4aQueryInputReachesProviderThroughTruncatedHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	ctx := context.Background()
	db, pool, botID, sessionID := r4aOpenHistory(t)
	seedDirectLifecycleModel(t, ctx, pool)
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	alice := r4aSender(t, ctx, pool, "Alice")
	for i := range 3 {
		r4aPersistRow(t, ctx, db, pool, botID, sessionID, fmt.Sprintf("m-old-%d", i), alice,
			fmt.Sprintf("old-%d ", i)+strings.Repeat("padding ", 400), at.Add(time.Duration(i)*time.Minute))
	}

	fixture, recorder := newDriveRequestFixture(t)
	withDriveRequestRuntime(t, fixture)
	fixture.service.messageService = &pgContextMessages{db: db, sessionID: sessionID}
	// A 400-token window loads 1600 bytes of content, far below the rows above,
	// and vision lets the attached image route natively.
	queries := fixture.service.queries.(noDecisionQueries).directLifecycleQueries
	for key, model := range queries.models {
		model.Config = []byte(`{"context_window":400,"compatibilities":["tool-call","vision"]}`)
		queries.models[key] = model
	}

	const prompt = "R4A_CURRENT_INPUT"
	handle, err := fixture.service.StartTurn(ctx, turn.StartTurnCommand{
		SchemaVersion:           1,
		TeamID:                  "r4a-team",
		Mode:                    turn.ModeChat,
		BotID:                   lifecycleTestBotID,
		ChatID:                  lifecycleTestBotID,
		ThreadID:                lifecycleTestSessionID,
		Query:                   prompt,
		UserVisibleText:         prompt,
		ExternalMessageID:       "r4a-current",
		SourceChannelIdentityID: "r4a-sender-carol",
		DisplayName:             "Carol",
		CurrentChannel:          "telegram",
		Channels:                []string{"telegram"},
		ReplyTarget:             "r4a-chat",
		ConversationType:        "private",
		Attachments:             []turn.Attachment{{Type: "image", Base64: dataURL("image/png", rasterPNG(t)), Mime: "image/png", Name: "photo.png"}},
	})
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	drainDriveHandle(t, handle)

	requests := requireDriveRequests(t, recorder)
	text := strings.Join(r4aSdkUserText(requests[0]), "\n")
	if !strings.Contains(text, prompt) {
		t.Fatalf("the run's input never reached the provider request:\n%s", text)
	}
	if !strings.Contains(text, `id="r4a-current"`) || !strings.Contains(text, `sender="Carol"`) {
		t.Fatalf("the current input reached the provider without its own source:\n%s", text)
	}
	if strings.Contains(text, "old-0") {
		t.Fatal("the byte budget dropped nothing, so the truncation this test rests on never happened")
	}
	if got := r4aSdkUserImages(requests[0]); got != 1 {
		t.Fatalf("image parts on the request = %d, want the attached image to ride the current input", got)
	}
}

// The row the request names comes back even when the byte budget spent itself
// on newer rows that the request's own cutoff then removed: the load can leave
// the row out entirely, and the context still carries it, because it is named
// by id and re-read rather than looked for in what the load returned.
func TestR4aNamedRowReturnsAfterTheLoadLeavesItOut(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	ctx := context.Background()
	db, pool, botID, sessionID := r4aOpenHistory(t)
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	carol := r4aSender(t, ctx, pool, "Carol")

	currentID := r4aPersistRow(t, ctx, db, pool, botID, sessionID, "m-current", carol, "the message the bot answers", at)
	// A newer row, past the request's cutoff, large enough to spend the whole
	// byte budget on its own.
	cutoffID := r4aPersistRow(t, ctx, db, pool, botID, sessionID, "m-replaced", carol, "replaced "+strings.Repeat("padding ", 2000), at.Add(time.Minute))

	assembled := r4aAssemble(t, db, botID, sessionID, ChatRequest{
		RequiredHistoryMessageID:     currentID,
		ReusePersistedUserMessage:    true,
		HistoryCutoffBeforeMessageID: cutoffID,
	}, 100)

	current := r4aFind(assembled.Records, currentID)
	if current == nil {
		t.Fatalf("the row the request named never came back: %v", stage4Labels(assembled.Messages))
	}
	if !current.Required {
		t.Fatal("the re-read row did not come back as the required one")
	}
	if r4aFind(assembled.Records, cutoffID) != nil {
		t.Fatal("a row past the request's cutoff reached the context")
	}
}

// Two rows carrying the same text are still told apart by what the run
// answered: the header names the row the request named, not whichever row
// happens to hold matching text.
func TestR4aSameTextDoesNotStandInForTheCurrentRow(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	ctx := context.Background()
	db, pool, botID, sessionID := r4aOpenHistory(t)
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	alice, carol := r4aSender(t, ctx, pool, "Alice"), r4aSender(t, ctx, pool, "Carol")

	const sameText = "is the release still on?"
	r4aPersistRow(t, ctx, db, pool, botID, sessionID, "m-earlier", alice, sameText, at)
	currentID := r4aPersistRow(t, ctx, db, pool, botID, sessionID, "m-current", carol, sameText, at.Add(time.Minute))

	assembled := r4aAssemble(t, db, botID, sessionID, ChatRequest{
		RequiredHistoryMessageID:  currentID,
		ReusePersistedUserMessage: true,
	}, 100000)

	current := r4aFind(assembled.Records, currentID)
	if current == nil {
		t.Fatal("the row the request named is not in the context")
	}
	if got := current.ExternalMessageID; got != "m-current" {
		t.Fatalf("current row external message id = %q, want m-current", got)
	}
	if got := current.SenderChannelIdentityID; got != carol {
		t.Fatalf("current row sender = %q, want %q", got, carol)
	}
	for _, record := range assembled.Records {
		if strings.TrimSpace(record.ExternalMessageID) != "m-earlier" {
			continue
		}
		if strings.TrimSpace(record.SenderChannelIdentityID) != alice {
			t.Fatalf("the earlier row lost its own sender: %q", record.SenderChannelIdentityID)
		}
	}
}
