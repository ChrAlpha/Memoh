package transcript

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	compaction "github.com/felinics/memoh/internal/agent/context/compaction"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/partmeta"
	"github.com/felinics/memoh/internal/agent/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

func TestDropPreflightFailureTurns(t *testing.T) {
	user := runreq.SDKMessagesToModelMessages([]sdk.Message{sdk.UserMessage("rejected")})[0]
	assistant := runreq.SDKMessagesToModelMessages([]sdk.Message{sdk.AssistantMessage("")})[0]
	kept := runreq.SDKMessagesToModelMessages([]sdk.Message{sdk.UserMessage("accepted")})[0]
	records := []historyfrag.HistoryRecord{
		{ModelMessage: user, Metadata: map[string]any{messagepkg.HistoryFailureOriginMetadataKey: messagepkg.HistoryFailureOriginUserMessageHook}},
		{ModelMessage: assistant, Metadata: map[string]any{messagepkg.HistoryFailureOriginMetadataKey: messagepkg.HistoryFailureOriginUserMessageHook}},
		{ModelMessage: kept},
	}

	got := dropPreflightFailureTurns(records)
	if len(got) != 1 || got[0].ModelMessage.TextContent() != "accepted" {
		t.Fatalf("preflight failure records = %#v, want only accepted input", got)
	}
}

func TestDropEmptyHistoryFailures(t *testing.T) {
	empty := runreq.SDKMessagesToModelMessages([]sdk.Message{sdk.AssistantMessage("")})[0]
	kept := runreq.SDKMessagesToModelMessages([]sdk.Message{sdk.AssistantMessage("hello")})[0]
	records := []historyfrag.HistoryRecord{
		{ModelMessage: empty, Metadata: map[string]any{messagepkg.HistoryErrorCodeMetadataKey: "agent.response_timeout"}},
		{ModelMessage: kept, Metadata: map[string]any{messagepkg.HistoryErrorCodeMetadataKey: "agent.response_timeout"}},
		{ModelMessage: empty},
	}
	got := dropEmptyHistoryFailures(records)
	if len(got) != 2 {
		t.Fatalf("kept %d records, want 2", len(got))
	}
	if got[0].ModelMessage.TextContent() != "hello" {
		t.Fatalf("first kept record = %#v", got[0].ModelMessage)
	}
	if strings.TrimSpace(got[1].ModelMessage.TextContent()) != "" || historyErrorCode(got[1].Metadata) != "" {
		t.Fatalf("empty unmarked assistant should stay, got %#v", got[1])
	}
}

func TestProjectInterruptedHistoryReasoning(t *testing.T) {
	records := []historyfrag.HistoryRecord{{
		ModelMessage: runreq.SDKMessagesToModelMessages([]sdk.Message{{
			Role: sdk.MessageRoleAssistant,
			Content: []sdk.MessagePart{
				sdk.ReasoningPart{Text: "partial reasoning"}, sdk.TextPart{Text: "partial text"},
			},
		}})[0],
		Metadata: map[string]any{messagepkg.AgentStepInterruptedMetadataKey: true},
	}}
	got := runreq.ModelMessageToSDKMessage(projectInterruptedHistoryReasoning(records)[0].ModelMessage)
	if len(got.Content) != 1 || got.Content[0].(sdk.TextPart).Text !=
		messagepkg.AgentStepInterruptedReasoningPrefix+"partial reasoning\n\npartial text" {
		t.Fatalf("projected content = %#v", got.Content)
	}
	if len(runreq.ModelMessageToSDKMessage(records[0].ModelMessage).Content) != 2 {
		t.Fatal("input records were mutated")
	}
}

func TestProjectInterruptedHistoryReasoningKeepsOpaqueBlock(t *testing.T) {
	checkpoint := historyfrag.HistoryRecord{
		ModelMessage: runreq.SDKMessagesToModelMessages([]sdk.Message{{
			Role: sdk.MessageRoleAssistant,
			Content: []sdk.MessagePart{sdk.ReasoningPart{
				ID:     "r1",
				Format: sdk.ReasoningFormatAnthropic,
				Model:  "claude-sonnet-4-20250514",
				ProviderMetadata: partmeta.Fold(map[string]any{
					"anthropic": map[string]any{"redactedData": "BLOB"},
				}),
			}},
		}})[0],
		Metadata: map[string]any{messagepkg.AgentStepInterruptedMetadataKey: true},
	}

	got := runreq.ModelMessageToSDKMessage(projectInterruptedHistoryReasoning([]historyfrag.HistoryRecord{checkpoint})[0].ModelMessage)
	if len(got.Content) != 1 {
		t.Fatalf("projected content = %#v, want one opaque reasoning block", got.Content)
	}
	part, ok := got.Content[0].(sdk.ReasoningPart)
	if !ok {
		t.Fatalf("content[0] = %T, want ReasoningPart", got.Content[0])
	}
	if part.ID != "r1" || part.Format != sdk.ReasoningFormatAnthropic ||
		part.Model != "claude-sonnet-4-20250514" {
		t.Fatalf("reasoning provenance was not preserved: %#v", part)
	}
	meta := part.ProviderMetadata["anthropic"]
	if data := meta["redactedData"]; data != "BLOB" {
		t.Fatalf("redactedData = %q, want BLOB", data)
	}
}

func TestProjectInterruptedHistoryReasoningSkipsSupersededCheckpoint(t *testing.T) {
	checkpoint := historyfrag.HistoryRecord{
		ModelMessage: runreq.SDKMessagesToModelMessages([]sdk.Message{{
			Role:    sdk.MessageRoleAssistant,
			Content: []sdk.MessagePart{sdk.ReasoningPart{Text: "partial reasoning"}},
		}})[0],
		Metadata: map[string]any{messagepkg.AgentStepInterruptedMetadataKey: true},
	}
	answer := historyfrag.HistoryRecord{
		ModelMessage: runreq.SDKMessagesToModelMessages([]sdk.Message{{
			Role:    sdk.MessageRoleAssistant,
			Content: []sdk.MessagePart{sdk.TextPart{Text: "the answer"}},
		}})[0],
	}
	projected := projectInterruptedHistoryReasoning([]historyfrag.HistoryRecord{checkpoint, answer})
	got := runreq.ModelMessageToSDKMessage(projected[0].ModelMessage)
	if len(got.Content) != 1 {
		t.Fatalf("projected content = %#v, want the reasoning part untouched", got.Content)
	}
	if _, ok := got.Content[0].(sdk.ReasoningPart); !ok {
		t.Fatalf("superseded checkpoint was projected into prompt text: %#v", got.Content[0])
	}
}

const (
	pipelineTestBotID     = "11111111-1111-1111-1111-111111111111"
	pipelineTestSessionID = "22222222-2222-2222-2222-222222222222"
)

type fakeArtifactLineageQueries struct {
	dbstore.Queries
	rows []sqlc.BotHistoryMessageCompact
}

func (f fakeArtifactLineageQueries) ListCompactionArtifactLineageBySession(context.Context, pgtype.UUID) ([]sqlc.BotHistoryMessageCompact, error) {
	return f.rows, nil
}

func (fakeArtifactLineageQueries) GetCompactionLogByID(context.Context, pgtype.UUID) (sqlc.BotHistoryMessageCompact, error) {
	return sqlc.BotHistoryMessageCompact{}, pgx.ErrNoRows
}

func (fakeArtifactLineageQueries) ListCompactionArtifactParentIDsBySuccessor(context.Context, sqlc.ListCompactionArtifactParentIDsBySuccessorParams) ([]pgtype.UUID, error) {
	return nil, nil
}

func compactionLogRow(t *testing.T, summary string, coveredExternalID string, createdAtMs int64) sqlc.BotHistoryMessageCompact {
	t.Helper()
	coverage, err := json.Marshal([]compaction.CoveredSource{{
		Ref: contextfrag.ContextRef{
			Namespace:   "bot_history_message",
			ID:          "33333333-3333-3333-3333-333333333333",
			Schema:      contextfrag.SchemaContextRef,
			HashAlgo:    contextfrag.HashAlgoSHA256,
			HashScope:   contextfrag.HashScopeSourcePayload,
			ContentHash: "deadbeef",
		},
		ExternalMessageID: coveredExternalID,
		CreatedAtMs:       createdAtMs,
	}})
	if err != nil {
		t.Fatalf("encode coverage: %v", err)
	}
	id, err := dbpkg.ParseUUID("44444444-4444-4444-4444-444444444444")
	if err != nil {
		t.Fatalf("parse artifact id: %v", err)
	}
	botID, err := dbpkg.ParseUUID(pipelineTestBotID)
	if err != nil {
		t.Fatalf("parse bot id: %v", err)
	}
	sessionID, err := dbpkg.ParseUUID(pipelineTestSessionID)
	if err != nil {
		t.Fatalf("parse session id: %v", err)
	}
	return sqlc.BotHistoryMessageCompact{
		ID:            id,
		BotID:         botID,
		SessionID:     sessionID,
		Status:        "ok",
		Summary:       summary,
		Coverage:      coverage,
		AnchorStartMs: createdAtMs,
		AnchorEndMs:   createdAtMs,
	}
}
