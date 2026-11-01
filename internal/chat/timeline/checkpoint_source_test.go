package timeline

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/chat/checkpoint"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

const checkpointTestSessionID = "66666666-6666-6666-6666-666666666666"

// fakeCheckpointQueries serves a session's active checkpoint, which holds,
// the texts of its summaries, the inbound messages with rows after its
// position, and how it accounts for the others, recording what was asked.
type fakeCheckpointQueries struct {
	dbstore.Queries
	active       sqlc.BotHistoryCheckpoint
	summaries    map[string]string
	after        []string
	accounted    []sqlc.ListHistoryCheckpointAccountedMessagesRow
	afterReads   []sqlc.ListHistoryMessageSourcesAfterParams
	accountReads []sqlc.ListHistoryCheckpointAccountedMessagesParams
}

func (f *fakeCheckpointQueries) GetActiveHistoryCheckpoint(context.Context, pgtype.UUID) (sqlc.BotHistoryCheckpoint, error) {
	if !f.active.ID.Valid {
		return sqlc.BotHistoryCheckpoint{}, pgx.ErrNoRows
	}
	return f.active, nil
}

func (f *fakeCheckpointQueries) ListHistoryCheckpointSummaries(_ context.Context, arg sqlc.ListHistoryCheckpointSummariesParams) ([]sqlc.ListHistoryCheckpointSummariesRow, error) {
	rows := make([]sqlc.ListHistoryCheckpointSummariesRow, 0, len(arg.Ids))
	for _, id := range arg.Ids {
		if text, ok := f.summaries[id.String()]; ok {
			rows = append(rows, sqlc.ListHistoryCheckpointSummariesRow{ID: id, Summary: text})
		}
	}
	return rows, nil
}

func (f *fakeCheckpointQueries) ListHistoryMessageSourcesAfter(_ context.Context, arg sqlc.ListHistoryMessageSourcesAfterParams) ([]string, error) {
	f.afterReads = append(f.afterReads, arg)
	return f.after, nil
}

func (f *fakeCheckpointQueries) ListHistoryCheckpointAccountedMessages(_ context.Context, arg sqlc.ListHistoryCheckpointAccountedMessagesParams) ([]sqlc.ListHistoryCheckpointAccountedMessagesRow, error) {
	f.accountReads = append(f.accountReads, arg)
	var rows []sqlc.ListHistoryCheckpointAccountedMessagesRow
	for _, row := range f.accounted {
		if slices.Contains(arg.ExternalMessageIds, row.ExternalMessageID) {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

type fakeCheckpointRows struct {
	messages []messagepkg.Message
	ids      []string
}

func (f *fakeCheckpointRows) ListTurnResponseSourcesByIDs(_ context.Context, _ string, ids []string) ([]messagepkg.Message, error) {
	f.ids = ids
	return f.messages, nil
}

func mustCheckpointUUID(t *testing.T, id string) pgtype.UUID {
	t.Helper()
	parsed, err := dbpkg.ParseUUID(id)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func checkpointRow(t *testing.T, position messagepkg.HistoryPosition, items ...checkpoint.Item) sqlc.BotHistoryCheckpoint {
	t.Helper()
	encoded, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	return sqlc.BotHistoryCheckpoint{
		ID:             mustCheckpointUUID(t, "cccccccc-cccc-cccc-cccc-cccccccccccc"),
		SessionID:      mustCheckpointUUID(t, checkpointTestSessionID),
		TurnPosition:   position.Turn,
		TurnMessageSeq: position.Seq,
		Items:          encoded,
	}
}

func storedModelMessage(t *testing.T, role string, content any) []byte {
	t.Helper()
	parts, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	message, err := json.Marshal(turn.ModelMessage{Role: role, Content: parts})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

// Load projects the active checkpoint as composition renders it: a summary
// with its text, a kept input row as the inbound message it records, a kept
// response as its turn response, and which inbound messages of the rendered
// context it folds. A kept user row that records no inbound message renders
// nothing.
func TestCheckpointSourceLoadProjectsTheActiveCheckpoint(t *testing.T) {
	const (
		summaryID  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa1"
		inputID    = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbb1"
		feedbackID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbb2"
		replyID    = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbb3"
		resultID   = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbb4"
	)
	queries := &fakeCheckpointQueries{
		active: checkpointRow(t, messagepkg.HistoryPosition{Turn: 4, Seq: 3},
			checkpoint.Item{SummaryID: summaryID},
			checkpoint.Item{RowID: inputID},
			checkpoint.Item{RowID: feedbackID},
			checkpoint.Item{RowID: replyID},
			checkpoint.Item{RowID: resultID},
		),
		summaries: map[string]string{summaryID: "folded work"},
		accounted: []sqlc.ListHistoryCheckpointAccountedMessagesRow{{ExternalMessageID: "m-input", Kept: true}, {ExternalMessageID: "m-folded", EventCursor: 7}},
	}
	rows := &fakeCheckpointRows{messages: []messagepkg.Message{
		{ID: inputID, Role: "user", ExternalMessageID: "m-input", Content: storedModelMessage(t, "user", "kept input")},
		{ID: feedbackID, Role: "user", Content: storedModelMessage(t, "user", "internal feedback")},
		{ID: replyID, Role: "assistant", CreatedAt: time.UnixMilli(1500), Content: storedModelMessage(t, "assistant", "kept reply")},
		{ID: resultID, Role: "tool", CreatedAt: time.UnixMilli(1600), Content: storedModelMessage(t, "tool", []map[string]any{{
			"type": "tool-result", "toolCallId": "call-1", "toolName": "read", "output": "file body",
		}})},
	}}
	source := NewCheckpointSource(slog.New(slog.DiscardHandler), queries, rows)
	rc := RenderedContext{textSegment("m-folded", 900, "folded input"), textSegment("m-input", 1000, "kept input"), textSegment("", 1100, "unnamed")}

	held, err := source.Load(context.Background(), checkpointTestSessionID, rc)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if held.Position != (messagepkg.HistoryPosition{Turn: 4, Seq: 3}) {
		t.Fatalf("position %+v, want the checkpoint's", held.Position)
	}
	if want := []string{inputID, feedbackID, replyID, resultID}; !reflect.DeepEqual(rows.ids, want) {
		t.Fatalf("kept rows read = %v, want %v", rows.ids, want)
	}
	if len(held.Items) != 4 {
		t.Fatalf("items = %+v, want the summary, the input and the two responses", held.Items)
	}
	if item := held.Items[0]; item.SummaryID != summaryID || item.Summary != "folded work" {
		t.Fatalf("item 0 = %+v, want the summary with its text", item)
	}
	if item := held.Items[1]; item.MessageID != "m-input" || item.Response != nil {
		t.Fatalf("item 1 = %+v, want the kept input's message", item)
	}
	if item := held.Items[2]; item.Response == nil || item.Response.SourceMessageID != replyID || item.Response.Role != "assistant" || item.Response.Content != "kept reply" {
		t.Fatalf("item 2 = %+v, want the kept reply", item)
	}
	if item := held.Items[3]; item.Response == nil || item.Response.SourceMessageID != resultID || item.Response.Role != "tool" {
		t.Fatalf("item 3 = %+v, want the kept tool result", item)
	}

	if len(queries.afterReads) != 1 || len(queries.accountReads) != 1 {
		t.Fatalf("reads after the position = %d, of what it accounts for = %d, want 1 each", len(queries.afterReads), len(queries.accountReads))
	}
	if after := queries.afterReads[0]; after.AfterTurnPosition != 4 || after.AfterTurnMessageSeq != 3 || after.SessionID.String() != checkpointTestSessionID {
		t.Fatalf("rows read after %d/%d in %s, want after the checkpoint's position in the session", after.AfterTurnPosition, after.AfterTurnMessageSeq, after.SessionID)
	}
	account := queries.accountReads[0]
	if account.ThroughTurnPosition != 4 || account.ThroughTurnMessageSeq != 3 || account.SessionID.String() != checkpointTestSessionID {
		t.Fatalf("rows accounted through %d/%d in %s, want through the checkpoint's position in the session", account.ThroughTurnPosition, account.ThroughTurnMessageSeq, account.SessionID)
	}
	kept := make([]string, 0, len(account.KeptIds))
	for _, id := range account.KeptIds {
		kept = append(kept, id.String())
	}
	if !reflect.DeepEqual(kept, rows.ids) {
		t.Fatalf("accounted kept ids = %v, want %v", kept, rows.ids)
	}
	if want := []string{"m-folded", "m-input"}; !reflect.DeepEqual(account.ExternalMessageIds, want) {
		t.Fatalf("accounted messages = %v, want %v", account.ExternalMessageIds, want)
	}
	if want := map[string]FoldedMessage{"m-folded": {EventCursor: 7}}; !reflect.DeepEqual(held.Folded, want) {
		t.Fatalf("folded = %v, want %v", held.Folded, want)
	}

	// A rendered context without inbound messages has nothing to account for.
	held, err = source.Load(context.Background(), checkpointTestSessionID, RenderedContext{textSegment("", 1100, "unnamed")})
	if err != nil || len(held.Items) != 4 || held.Folded != nil || len(queries.afterReads) != 1 || len(queries.accountReads) != 1 {
		t.Fatalf("held=%+v err=%v reads=%d/%d, want the items without reading what it folds", held, err, len(queries.afterReads), len(queries.accountReads))
	}
}

// What the checkpoint's rows at or before its position say is read once per
// message while it stays active; the rows after it are read on every load,
// so a folded message that gains one turns raw and a raw one that loses its
// rows there is accounted for. Another checkpoint reads everything again.
func TestCheckpointSourceReadsWhatItAccountsForOncePerCheckpoint(t *testing.T) {
	const summaryID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa1"
	queries := &fakeCheckpointQueries{
		active:    checkpointRow(t, messagepkg.HistoryPosition{Turn: 4, Seq: 3}, checkpoint.Item{SummaryID: summaryID}),
		summaries: map[string]string{summaryID: "folded work"},
		after:     []string{"m-raw"},
		accounted: []sqlc.ListHistoryCheckpointAccountedMessagesRow{
			{ExternalMessageID: "m1", EventCursor: 1}, {ExternalMessageID: "m2", EventCursor: 2}, {ExternalMessageID: "m-raw", EventCursor: 3},
		},
	}
	source := NewCheckpointSource(slog.New(slog.DiscardHandler), queries, &fakeCheckpointRows{})
	load := func(ids ...string) map[string]FoldedMessage {
		t.Helper()
		rc := make(RenderedContext, 0, len(ids))
		for i, id := range ids {
			rc = append(rc, textSegment(id, int64(1000+i), id))
		}
		held, err := source.Load(context.Background(), checkpointTestSessionID, rc)
		if err != nil {
			t.Fatal(err)
		}
		return held.Folded
	}
	accountedLast := func() []string {
		return queries.accountReads[len(queries.accountReads)-1].ExternalMessageIds
	}

	if got, want := load("m1", "m2", "m-raw", "m-new"), map[string]FoldedMessage{"m1": {EventCursor: 1}, "m2": {EventCursor: 2}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first load folds %v, want %v", got, want)
	}
	if want := []string{"m1", "m2", "m-new"}; !reflect.DeepEqual(accountedLast(), want) {
		t.Fatalf("first load accounted for %v, want every message without rows after the position %v", accountedLast(), want)
	}

	queries.after = []string{"m-raw", "m2"}
	if got, want := load("m1", "m2", "m-raw", "m-new", "m3"), map[string]FoldedMessage{"m1": {EventCursor: 1}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("second load folds %v, want m2 raw once it has a row after the position: %v", got, want)
	}
	if want := []string{"m3"}; len(queries.accountReads) != 2 || !reflect.DeepEqual(accountedLast(), want) {
		t.Fatalf("second load accounted for %v, want only the new message %v", accountedLast(), want)
	}

	queries.after = nil
	if got, want := load("m1", "m2", "m-raw"), map[string]FoldedMessage{"m1": {EventCursor: 1}, "m2": {EventCursor: 2}, "m-raw": {EventCursor: 3}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("third load folds %v, want every message without rows after the position %v", got, want)
	}
	if want := []string{"m-raw"}; len(queries.accountReads) != 3 || !reflect.DeepEqual(accountedLast(), want) {
		t.Fatalf("third load accounted for %v, want only the message that lost its rows after the position %v", accountedLast(), want)
	}

	queries.active.ID = mustCheckpointUUID(t, "dddddddd-dddd-dddd-dddd-dddddddddddd")
	load("m1", "m2")
	if want := []string{"m1", "m2"}; len(queries.accountReads) != 4 || !reflect.DeepEqual(accountedLast(), want) {
		t.Fatalf("another checkpoint accounted for %v, want everything again %v", accountedLast(), want)
	}
	if len(queries.afterReads) != 4 {
		t.Fatalf("rows after the position read %d times, want once per load", len(queries.afterReads))
	}
}

func TestCheckpointSourceLoadWithoutCheckpoint(t *testing.T) {
	queries := &fakeCheckpointQueries{}
	rows := &fakeCheckpointRows{}
	held, err := NewCheckpointSource(slog.New(slog.DiscardHandler), queries, rows).
		Load(context.Background(), checkpointTestSessionID, RenderedContext{textSegment("m1", 1000, "hello")})
	if err != nil || !reflect.DeepEqual(held, HistoryCheckpoint{}) {
		t.Fatalf("held=%+v err=%v, want the zero checkpoint", held, err)
	}
	if rows.ids != nil || len(queries.afterReads) != 0 || len(queries.accountReads) != 0 {
		t.Fatalf("rows read %v, reads %d/%d, want none", rows.ids, len(queries.afterReads), len(queries.accountReads))
	}
}
