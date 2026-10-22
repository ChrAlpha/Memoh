package transcript

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// assembleCurrentInput reads one run's context through the shared entry point.
func assembleCurrentInput(s *stage4Session, chat runreq.ChatRequest, budget int) Assembled {
	s.T.Helper()
	assembled, err := s.assembleFor(PurposeRun, chat, budget)
	if err != nil {
		s.T.Fatalf("Assemble: %v", err)
	}
	return assembled
}

// persistSender creates a channel identity so a row can carry a real sender, and
// returns its id.
func persistSender(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) string {
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

// persistInputRow writes one user row carrying the identity a channel writer
// records, at a chosen point on the fixture's own clock so the load order is
// the test's.
func persistInputRow(t *testing.T, ctx context.Context, db *messagepkg.DBService, pool *pgxpool.Pool, botID, sessionID, externalID, senderID, text string, at time.Time) string {
	t.Helper()
	content, err := json.Marshal(runreq.SDKMessagesToModelMessages([]sdk.Message{sdk.UserMessage(text)})[0])
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

func findRecord(records []historyfrag.HistoryRecord, dbMessageID string) *historyfrag.HistoryRecord {
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
func TestPostgresCurrentRowStaysInContextUnderByteBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	s := newStage4Session(t)
	ctx, db, pool, botID, sessionID := s.Ctx, s.Messages, s.Pool, s.BotID, s.SessionID
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	alice, carol := persistSender(t, ctx, pool, "Alice"), persistSender(t, ctx, pool, "Carol")

	persistInputRow(t, ctx, db, pool, botID, sessionID, "m-old-a", alice, "old-a "+strings.Repeat("padding ", 400), at)
	persistInputRow(t, ctx, db, pool, botID, sessionID, "m-old-b", alice, "old-b "+strings.Repeat("padding ", 400), at.Add(time.Minute))
	currentID := persistInputRow(t, ctx, db, pool, botID, sessionID, "m-current", carol, "current input", at.Add(2*time.Minute))

	// 400 tokens is 1600 bytes of content, below the two old rows alone.
	assembled := assembleCurrentInput(s, runreq.ChatRequest{
		RequiredHistoryMessageID:  currentID,
		ReusePersistedUserMessage: true,
	}, 400)

	current := findRecord(assembled.Records, currentID)
	if current == nil {
		t.Fatalf("the row the request named was dropped by the byte budget: %v", agenttest.CompactionLabels(assembled.Messages))
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
func TestPostgresCurrentRowIdentitySurvivesLaterMessages(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	s := newStage4Session(t)
	ctx, db, pool, botID, sessionID := s.Ctx, s.Messages, s.Pool, s.BotID, s.SessionID
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	alice, carol, bob := persistSender(t, ctx, pool, "Alice"), persistSender(t, ctx, pool, "Carol"), persistSender(t, ctx, pool, "Bob")

	persistInputRow(t, ctx, db, pool, botID, sessionID, "m-old", alice, "old "+strings.Repeat("padding ", 200), at)
	currentID := persistInputRow(t, ctx, db, pool, botID, sessionID, "m-current", carol, "the message the bot answers", at.Add(time.Minute))
	persistInputRow(t, ctx, db, pool, botID, sessionID, "m-later", bob, "a newer message from somebody else", at.Add(2*time.Minute))

	assembled := assembleCurrentInput(s, runreq.ChatRequest{
		RequiredHistoryMessageID:  currentID,
		ReusePersistedUserMessage: true,
	}, 100000)

	current := findRecord(assembled.Records, currentID)
	if current == nil {
		t.Fatalf("the row the request named is not in the context: %v", agenttest.CompactionLabels(assembled.Messages))
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
func TestPostgresOversizedCurrentRowIsSentNotDropped(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	s := newStage4Session(t)
	ctx, db, pool, botID, sessionID := s.Ctx, s.Messages, s.Pool, s.BotID, s.SessionID
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	alice, carol := persistSender(t, ctx, pool, "Alice"), persistSender(t, ctx, pool, "Carol")

	persistInputRow(t, ctx, db, pool, botID, sessionID, "m-old", alice, "old "+strings.Repeat("padding ", 400), at)
	currentID := persistInputRow(t, ctx, db, pool, botID, sessionID, "m-current", carol, "current "+strings.Repeat("oversized ", 5000), at.Add(time.Minute))

	assembled := assembleCurrentInput(s, runreq.ChatRequest{
		RequiredHistoryMessageID:  currentID,
		ReusePersistedUserMessage: true,
	}, 100)

	current := findRecord(assembled.Records, currentID)
	if current == nil {
		t.Fatalf("an oversized current input was dropped instead of being sent: %v", agenttest.CompactionLabels(assembled.Messages))
	}
	if !strings.Contains(current.ModelMessage.TextContent(), "oversized") {
		t.Fatal("the current row reached the context without its text")
	}
	if !assembled.Truncated {
		t.Fatal("the load did not report that it stopped before the oldest row")
	}
}

// The row the request names comes back even when the byte budget spent itself
// on newer rows that the request's own cutoff then removed: the load can leave
// the row out entirely, and the context still carries it, because it is named
// by id and re-read rather than looked for in what the load returned.
func TestPostgresNamedRowReturnsAfterTheLoadLeavesItOut(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	s := newStage4Session(t)
	ctx, db, pool, botID, sessionID := s.Ctx, s.Messages, s.Pool, s.BotID, s.SessionID
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	carol := persistSender(t, ctx, pool, "Carol")

	currentID := persistInputRow(t, ctx, db, pool, botID, sessionID, "m-current", carol, "the message the bot answers", at)
	// A newer row, past the request's cutoff, large enough to spend the whole
	// byte budget on its own.
	cutoffID := persistInputRow(t, ctx, db, pool, botID, sessionID, "m-replaced", carol, "replaced "+strings.Repeat("padding ", 2000), at.Add(time.Minute))

	assembled := assembleCurrentInput(s, runreq.ChatRequest{
		RequiredHistoryMessageID:     currentID,
		ReusePersistedUserMessage:    true,
		HistoryCutoffBeforeMessageID: cutoffID,
	}, 100)

	current := findRecord(assembled.Records, currentID)
	if current == nil {
		t.Fatalf("the row the request named never came back: %v", agenttest.CompactionLabels(assembled.Messages))
	}
	if !current.Required {
		t.Fatal("the re-read row did not come back as the required one")
	}
	if findRecord(assembled.Records, cutoffID) != nil {
		t.Fatal("a row past the request's cutoff reached the context")
	}
}

// Two rows carrying the same text are still told apart by what the run
// answered: the header names the row the request named, not whichever row
// happens to hold matching text.
func TestPostgresSameTextDoesNotStandInForTheCurrentRow(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	s := newStage4Session(t)
	ctx, db, pool, botID, sessionID := s.Ctx, s.Messages, s.Pool, s.BotID, s.SessionID
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	alice, carol := persistSender(t, ctx, pool, "Alice"), persistSender(t, ctx, pool, "Carol")

	const sameText = "is the release still on?"
	persistInputRow(t, ctx, db, pool, botID, sessionID, "m-earlier", alice, sameText, at)
	currentID := persistInputRow(t, ctx, db, pool, botID, sessionID, "m-current", carol, sameText, at.Add(time.Minute))

	assembled := assembleCurrentInput(s, runreq.ChatRequest{
		RequiredHistoryMessageID:  currentID,
		ReusePersistedUserMessage: true,
	}, 100000)

	current := findRecord(assembled.Records, currentID)
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
