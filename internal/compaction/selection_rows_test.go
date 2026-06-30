package compaction

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/conversation"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
	"github.com/memohai/memoh/internal/historyfrag"
)

func fixedUUID(value string) pgtype.UUID {
	id := uuid.MustParse(value)
	return pgtype.UUID{Bytes: id, Valid: true}
}

func TestRecordCandidatesFromRowsPreservesTurnDAGAndDirectedMetadata(t *testing.T) {
	t.Parallel()

	row := sqlc.ListUncompactedMessagesBySessionRow{
		ID:                      fixedUUID("00000000-0000-0000-0000-000000000001"),
		BotID:                   fixedUUID("00000000-0000-0000-0000-000000000002"),
		SessionID:               fixedUUID("00000000-0000-0000-0000-000000000003"),
		TurnID:                  fixedUUID("00000000-0000-0000-0000-000000000004"),
		ViewHeadTurnID:          fixedUUID("00000000-0000-0000-0000-000000000044"),
		TurnMessageSeq:          pgtype.Int8{Int64: 9, Valid: true},
		SenderChannelIdentityID: fixedUUID("00000000-0000-0000-0000-000000000005"),
		SenderUserID:            fixedUUID("00000000-0000-0000-0000-000000000006"),
		ExternalMessageID:       pgtype.Text{String: "external-1", Valid: true},
		SourceReplyToMessageID:  pgtype.Text{String: "external-0", Valid: true},
		Role:                    "user",
		Content:                 mustCompactionJSON(conversation.ModelMessage{Role: "user", Content: conversation.NewTextContent("hello")}),
		Metadata:                []byte(`{"trigger_mode":"direct"}`),
		Usage:                   []byte(`{"output_tokens":42}`),
		SessionMode:             "chat",
		RuntimeType:             "model",
		EventID:                 fixedUUID("00000000-0000-0000-0000-000000000007"),
		DisplayText:             pgtype.Text{String: "hello", Valid: true},
		CompactID:               pgtype.UUID{},
		CreatedAt:               pgtype.Timestamptz{Time: time.Date(2026, 6, 30, 1, 2, 3, 0, time.UTC), Valid: true},
		SenderDisplayName:       pgtype.Text{String: "Alice", Valid: true},
		SenderAvatarUrl:         pgtype.Text{String: "https://example/avatar.png", Valid: true},
		Platform:                pgtype.Text{String: "telegram", Valid: true},
		ConversationType:        pgtype.Text{String: "group", Valid: true},
		ConversationName:        "Ops Room",
		ReplyTarget:             pgtype.Text{String: "thread-9", Valid: true},
	}

	candidates, skipped := recordCandidatesFromRows([]sqlc.ListUncompactedMessagesBySessionRow{row}, "")
	if skipped != 0 || len(candidates) != 1 {
		t.Fatalf("candidates=%d skipped=%d, want one classified row", len(candidates), skipped)
	}
	record := candidates[0].Record
	if record.TurnID != "00000000-0000-0000-0000-000000000004" || record.TurnMessageSeq != 9 {
		t.Fatalf("record lost turn ordering: turn=%q seq=%d", record.TurnID, record.TurnMessageSeq)
	}
	if record.SessionMode != "chat" || record.RuntimeType != "model" {
		t.Fatalf("record lost session/runtime: mode=%q runtime=%q", record.SessionMode, record.RuntimeType)
	}
	if record.Scope.ViewHeadTurnID != "00000000-0000-0000-0000-000000000044" ||
		record.Scope.TurnID != record.TurnID ||
		record.Scope.TurnMessageSeq != record.TurnMessageSeq ||
		record.Scope.ConversationType != "group" ||
		record.Scope.ConversationName != "Ops Room" ||
		record.Scope.ReplyTarget != "thread-9" {
		t.Fatalf("record scope lost metadata: %#v", record.Scope)
	}
	if record.ExternalMessageID != "external-1" || record.SourceReplyToMessageID != "external-0" || record.SenderDisplayName != "Alice" || record.Platform != "telegram" {
		t.Fatalf("record lost directed metadata: %#v", record)
	}
	if got := estimateRecordCandidateTokens(candidates[0]); got != 42 {
		t.Fatalf("token estimate = %d, want 42 from snake-case usage", got)
	}
}

func TestRecordCandidatesFromRowsSkipsInvalidRows(t *testing.T) {
	t.Parallel()

	good := sqlc.ListUncompactedMessagesBySessionRow{
		ID:      fixedUUID("00000000-0000-0000-0000-000000000011"),
		BotID:   fixedUUID("00000000-0000-0000-0000-000000000012"),
		Role:    "user",
		Content: mustCompactionJSON(conversation.ModelMessage{Role: "user", Content: conversation.NewTextContent("ok")}),
	}
	bad := good
	bad.ID = pgtype.UUID{}

	candidates, skipped := recordCandidatesFromRows([]sqlc.ListUncompactedMessagesBySessionRow{bad, good}, "")
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1", skipped)
	}
	if len(candidates) != 1 || candidates[0].Record.Ref.ID != "00000000-0000-0000-0000-000000000011" {
		raw, _ := json.Marshal(candidates)
		t.Fatalf("unexpected candidates: %s", raw)
	}
}

func TestMessageIDsFromRecordRefsUsesAllSelectedRefs(t *testing.T) {
	t.Parallel()

	_, refs := buildRecordEntriesAndRefs(recordCandidatesFromRecords([]historyfrag.HistoryRecord{
		{
			Ref:          contextRef("00000000-0000-0000-0000-000000000021"),
			ModelMessage: conversation.ModelMessage{Role: "assistant", Content: mustCompactionJSON([]map[string]any{{"type": "reasoning", "text": "hidden"}})},
		},
		{
			Ref:          contextRef("00000000-0000-0000-0000-000000000022"),
			ModelMessage: conversation.ModelMessage{Role: "assistant", Content: conversation.NewTextContent("visible")},
		},
	}))

	ids, err := messageIDsFromRecordRefs(refs)
	if err != nil {
		t.Fatalf("messageIDsFromRecordRefs failed: %v", err)
	}
	if len(ids) != 2 || !ids[0].Valid || !ids[1].Valid {
		t.Fatalf("ids = %#v, want two valid ids", ids)
	}
	if uuid.UUID(ids[0].Bytes).String() != "00000000-0000-0000-0000-000000000021" ||
		uuid.UUID(ids[1].Bytes).String() != "00000000-0000-0000-0000-000000000022" {
		t.Fatalf("ids = %#v, want all selected refs in order", ids)
	}
}

func contextRef(id string) contextfrag.ContextRef {
	return contextfrag.ContextRef{
		Namespace:  historyfrag.NamespaceDBHistoryMessage,
		ID:         id,
		Schema:     contextfrag.SchemaContextRef,
		Durability: contextfrag.RefDurable,
	}
}
