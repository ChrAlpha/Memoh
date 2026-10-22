package persist

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/agentapi"
	"github.com/felinics/memoh/internal/agent/agenttest"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runreq"
	turnpkg "github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	textprune "github.com/felinics/memoh/internal/prune"
	"github.com/felinics/memoh/internal/settings"
)

func TestBuildInteractionMetadataIncludesForwardConversation(t *testing.T) {
	t.Parallel()

	meta := InteractionMetadata(runreq.ChatRequest{
		SourceReplyToMessageID:    "reply-1",
		ReplySender:               "Original Sender",
		ReplyPreview:              "quoted text",
		ForwardMessageID:          "forward-1",
		ForwardFromUserID:         "source-user",
		ForwardFromConversationID: "source-conversation",
		ForwardSender:             "Source Channel",
		ForwardDate:               1710000000,
	})

	reply, ok := meta["reply"].(map[string]any)
	if !ok || reply["message_id"] != "reply-1" || reply["sender"] != "Original Sender" || reply["preview"] != "quoted text" {
		t.Fatalf("unexpected reply metadata: %#v", meta["reply"])
	}
	forward, ok := meta["forward"].(map[string]any)
	if !ok {
		t.Fatalf("expected forward metadata: %#v", meta)
	}
	if forward["message_id"] != "forward-1" ||
		forward["from_user_id"] != "source-user" ||
		forward["from_conversation_id"] != "source-conversation" ||
		forward["sender"] != "Source Channel" ||
		forward["date"] != int64(1710000000) {
		t.Fatalf("unexpected forward metadata: %#v", forward)
	}
}

func TestBuildInteractionMetadataIncludesRequestedSkills(t *testing.T) {
	t.Parallel()

	meta := InteractionMetadata(runreq.ChatRequest{
		RequestedSkills: []runreq.RequestedSkillContext{
			{
				Name:           "writer",
				SourceKind:     "managed",
				OpaqueSourceID: "src-1",
				Content:        "raw content must not be persisted in metadata",
				ContentHash:    "hash-must-not-leak",
				Identity:       "managed:src-1:writer",
			},
			{
				Name:           "writer",
				SourceKind:     "managed",
				OpaqueSourceID: "src-1",
				Identity:       "managed:src-1:writer",
			},
			{
				Name:       "reviewer",
				SourceKind: "registry",
			},
		},
	})

	raw, ok := meta["model_requested_skills"].([]map[string]any)
	if !ok {
		t.Fatalf("expected requested skill metadata: %#v", meta["model_requested_skills"])
	}
	if len(raw) != 2 {
		t.Fatalf("expected deduped requested skills, got %#v", raw)
	}
	if raw[0]["name"] != "writer" || raw[0]["source_kind"] != "managed" {
		t.Fatalf("unexpected first skill metadata: %#v", raw[0])
	}
	if _, ok := raw[0]["opaque_source_id"]; ok {
		t.Fatalf("requested skill metadata leaked opaque source id: %#v", raw[0])
	}
	if _, ok := raw[0]["content"]; ok {
		t.Fatalf("requested skill metadata leaked content: %#v", raw[0])
	}
	if _, ok := raw[0]["content_hash"]; ok {
		t.Fatalf("requested skill metadata leaked content_hash: %#v", raw[0])
	}
	if _, ok := raw[0]["ref"]; ok {
		t.Fatalf("requested skill metadata leaked ref: %#v", raw[0])
	}
	if raw[1]["name"] != "reviewer" || raw[1]["source_kind"] != "registry" {
		t.Fatalf("unexpected second skill metadata: %#v", raw[1])
	}
}

func TestBuildInteractionMetadataIncludesPublicSkillActivation(t *testing.T) {
	t.Parallel()

	meta := InteractionMetadata(runreq.ChatRequest{
		UserMessageKind: runreq.UserMessageKindSkillActivation,
		SkillActivation: &runreq.SkillActivation{
			Prompt: "do it",
			Skills: []runreq.SkillActivationSkill{{
				Name:        "writer",
				DisplayName: "Writer",
				Description: "short safe description",
				SourceKind:  "managed",
				State:       "effective",
			}},
		},
		RequestedSkills: []runreq.RequestedSkillContext{{
			Name:           "writer",
			SourceKind:     "managed",
			OpaqueSourceID: "opaque-source",
			Content:        "raw content must not leak",
			ContentHash:    "hash-must-not-leak",
			Identity:       "writer|opaque-source|hash",
		}},
	})

	if meta["user_message_kind"] != runreq.UserMessageKindSkillActivation {
		t.Fatalf("user_message_kind = %#v", meta["user_message_kind"])
	}
	public, ok := meta["skill_activation"].(map[string]any)
	if !ok {
		t.Fatalf("expected public skill activation metadata: %#v", meta["skill_activation"])
	}
	if public["prompt"] != "do it" {
		t.Fatalf("prompt = %#v, want do it", public["prompt"])
	}
	skills, ok := public["skills"].([]map[string]any)
	if !ok || len(skills) != 1 {
		t.Fatalf("public skills = %#v, want one", public["skills"])
	}
	if skills[0]["name"] != "writer" || skills[0]["display_name"] != "Writer" {
		t.Fatalf("unexpected public skill: %#v", skills[0])
	}
	for _, key := range []string{"opaque_source_id", "content", "content_hash", "ref"} {
		if _, ok := skills[0][key]; ok {
			t.Fatalf("public skill leaked %s: %#v", key, skills[0])
		}
	}
	if _, ok := meta["audit_requested_skills"]; ok {
		t.Fatalf("audit metadata leaked into message metadata: %#v", meta["audit_requested_skills"])
	}
}

type batchRecordingMessageService struct {
	agenttest.RecordingMessages
	batchInputs []messagepkg.PersistInput
}

type lifecycleRecordingMessageService struct {
	agenttest.RecordingMessages
	messageIDs []string
}

func (s *lifecycleRecordingMessageService) Persist(_ context.Context, input messagepkg.PersistInput) (messagepkg.Message, error) {
	s.Persisted = append(s.Persisted, input)
	return messagepkg.Message{
		ID:       s.messageIDs[len(s.Persisted)-1],
		Role:     input.Role,
		Metadata: input.Metadata,
	}, nil
}

func (s *batchRecordingMessageService) PersistToolTailRound(_ context.Context, inputs []messagepkg.PersistInput) ([]messagepkg.Message, bool, error) {
	s.batchInputs = append(s.batchInputs, inputs...)
	return recordedMessages(inputs), true, nil
}

func TestStoreMessagesResultUsesToolTailBatch(t *testing.T) {
	t.Parallel()

	messages := &batchRecordingMessageService{}
	resolver := newTestWriter(Deps{Messages: messages})

	persisted, err := resolver.storeMessagesResult(context.Background(), runreq.ChatRequest{
		BotID:       storeRoundBotID,
		ThreadID:    "33333333-3333-3333-3333-333333333333",
		Query:       "hello",
		SessionType: "chat",
		RuntimeType: "model",
	}, []runreq.ModelMessage{
		{Role: "user", Content: runreq.NewTextContent("hello")},
		{Role: "assistant", Content: runreq.NewTextContent("call tool")},
		{Role: "tool", Content: runreq.NewTextContent("tool result")},
		{Role: "assistant", Content: runreq.NewTextContent("done")},
	}, "", RoundOptions{})
	if err != nil {
		t.Fatalf("storeMessagesResult() error = %v", err)
	}

	if len(messages.batchInputs) != 4 {
		t.Fatalf("batch inputs = %d, want 4", len(messages.batchInputs))
	}
	if len(messages.Persisted) != 0 {
		t.Fatalf("fallback Persist called %d times, want 0", len(messages.Persisted))
	}
	if len(persisted) != 4 {
		t.Fatalf("persisted messages = %d, want 4", len(persisted))
	}
}

func TestBuildPersistInputsKeepsAttachmentOnlyUserDisplayTextEmpty(t *testing.T) {
	t.Parallel()

	service := newTestWriter(Deps{})
	// resolve() replaces ChatRequest.Query with the headerified envelope before
	// the round is stored; an image with no caption leaves RawQuery empty.
	headerified := turnpkg.FormatUserHeader(turnpkg.UserMessageHeaderInput{
		DisplayName:      "User",
		Channel:          "web",
		ConversationType: "private",
		AttachmentPaths:  []string{"/data/.memoh/media/b2/b2edf40e.png"},
		Time:             time.Date(2026, 8, 20, 17, 37, 14, 0, time.UTC),
	}, "")

	inputs, err := service.buildPersistInputs(context.Background(), runreq.ChatRequest{
		BotID:    storeRoundBotID,
		ThreadID: "33333333-3333-3333-3333-333333333333",
		Query:    headerified,
	}, []runreq.ModelMessage{
		{Role: "user", Content: runreq.NewTextContent(headerified)},
		{Role: "assistant", Content: runreq.NewTextContent("nice picture")},
	}, "", RoundOptions{})
	if err != nil {
		t.Fatalf("buildPersistInputs() error = %v", err)
	}
	if len(inputs) != 2 {
		t.Fatalf("persist inputs = %d, want 2", len(inputs))
	}
	if got := inputs[0].DisplayText; got != "" {
		t.Fatalf("attachment-only user display text = %q, want empty", got)
	}
	if !bytes.Contains(inputs[0].Content, []byte("attachment path=")) {
		t.Fatalf("model-facing content lost its envelope: %s", inputs[0].Content)
	}
}

func TestBuildPersistInputsLiftsToolCallDiffsToRowMetadata(t *testing.T) {
	t.Parallel()

	service := newTestWriter(Deps{})
	diff := "--- a/f\n+++ b/f\n@@ -1 +1 @@\n-old\n+new\n"
	parts, err := json.Marshal([]map[string]any{{
		"type":       "tool-call",
		"toolCallId": "call-1",
		"toolName":   "edit",
		"input":      map[string]any{"path": "f"},
		"providerMetadata": map[string]any{
			"diff":               diff,
			"execution_location": map[string]any{"kind": "remote"},
		},
	}})
	if err != nil {
		t.Fatalf("marshal parts: %v", err)
	}

	inputs, err := service.buildPersistInputs(context.Background(), runreq.ChatRequest{
		BotID:    storeRoundBotID,
		ThreadID: "33333333-3333-3333-3333-333333333333",
	}, []runreq.ModelMessage{
		{Role: "assistant", Content: parts},
	}, "", RoundOptions{})
	if err != nil {
		t.Fatalf("buildPersistInputs() error = %v", err)
	}
	if len(inputs) != 1 {
		t.Fatalf("persist inputs = %d, want 1", len(inputs))
	}
	// The diff must not live in the content column — that is what the history
	// byte budget measures — while sibling providerMetadata stays put.
	if bytes.Contains(inputs[0].Content, []byte(`"diff"`)) {
		t.Fatalf("content still carries the diff: %s", inputs[0].Content)
	}
	if !bytes.Contains(inputs[0].Content, []byte(`"execution_location"`)) {
		t.Fatalf("content lost sibling providerMetadata: %s", inputs[0].Content)
	}
	diffs, ok := inputs[0].Metadata[messagepkg.ToolCallDiffsMetadataKey].(map[string]string)
	if !ok || diffs["call-1"] != diff {
		t.Fatalf("row metadata diffs = %#v", inputs[0].Metadata)
	}
}

func TestStoreRoundPersistsLifecycleMetadataOnLastAssistant(t *testing.T) {
	t.Parallel()

	holder := contextfrag.NewLifecycleHolder()
	holder.SetManifest(contextfrag.Manifest{
		View: contextfrag.ViewRunConfigPreProvider,
		Counts: contextfrag.ManifestCounts{
			Fragments: 3,
			Messages:  2,
			TextBytes: 96,
		},
		Items: []contextfrag.ManifestItem{{ID: "private-content-marker"}},
	})
	holder.SetMemoryRecall(contextfrag.MemoryRecallTrace{
		ProviderID:    "provider-1",
		CacheState:    "miss",
		RetrievalMode: "graph",
		Query: contextfrag.MemoryRecallQueryTrace{
			Source: "current_query",
		},
		Result: contextfrag.MemoryRecallResultTrace{
			Count:        1,
			Refs:         []string{"memory-1"},
			ContextBytes: 16,
		},
	})
	messages := &lifecycleRecordingMessageService{
		messageIDs: []string{"user-id", "first-assistant-id", " final-assistant-id "},
	}
	service := newTestWriter(Deps{Messages: messages})

	err := service.StoreRound(context.Background(), runreq.ChatRequest{
		BotID:    "bot-1",
		ThreadID: "session-1",
		Query:    "hello",
	}, []runreq.ModelMessage{
		{Role: "user", Content: runreq.NewTextContent("hello")},
		{Role: "assistant", Content: runreq.NewTextContent("first")},
		{Role: "assistant", Content: runreq.NewTextContent("final")},
	}, "model-1", RoundOptions{
		SkipMemory:       true,
		ContextLifecycle: holder,
		MessageMetadataByIndex: map[int]map[string]any{
			2: {"existing": "kept"},
		},
	})
	if err != nil {
		t.Fatalf("storeRoundWithOptions() error = %v", err)
	}
	if len(messages.Persisted) != 3 {
		t.Fatalf("persisted messages = %d, want 3", len(messages.Persisted))
	}
	if _, ok := messages.Persisted[1].Metadata[contextfrag.MetadataContextLifecycleKey]; ok {
		t.Fatalf("first assistant received lifecycle metadata: %#v", messages.Persisted[1].Metadata)
	}
	finalMeta := messages.Persisted[2].Metadata
	if finalMeta["existing"] != "kept" {
		t.Fatalf("existing metadata was not preserved: %#v", finalMeta)
	}
	metadataSnapshot, ok := finalMeta[contextfrag.MetadataContextLifecycleKey].(contextfrag.LifecycleSnapshot)
	if !ok {
		t.Fatalf("lifecycle metadata = %#v, want LifecycleSnapshot", finalMeta[contextfrag.MetadataContextLifecycleKey])
	}
	if metadataSnapshot.Version != contextfrag.LifecycleSnapshotVersion || metadataSnapshot.Counts.Messages != 2 {
		t.Fatalf("lifecycle metadata snapshot = %#v", metadataSnapshot)
	}
	raw, err := json.Marshal(metadataSnapshot)
	if err != nil {
		t.Fatalf("marshal lifecycle metadata: %v", err)
	}
	if strings.Contains(string(raw), "private-content-marker") || strings.Contains(string(raw), `"items"`) {
		t.Fatalf("lifecycle metadata leaked manifest items: %s", raw)
	}
	for _, want := range []string{"memory_recall", "provider-1", "memory-1"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("lifecycle metadata missing %q: %s", want, raw)
		}
	}

	snapshot, ok := holder.Snapshot()
	if !ok {
		t.Fatal("expected lifecycle snapshot after persistence")
	}
	if snapshot.AssistantMessageID != "final-assistant-id" {
		t.Fatalf("assistant message ID = %q, want final-assistant-id", snapshot.AssistantMessageID)
	}
}

func TestStoreRoundLogsSkippedLifecycleMetadataReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		holder     *contextfrag.LifecycleHolder
		messages   []runreq.ModelMessage
		wantReason string
	}{
		{
			name:       "missing lifecycle",
			messages:   []runreq.ModelMessage{{Role: "assistant", Content: runreq.NewTextContent("done")}},
			wantReason: "missing_lifecycle",
		},
		{
			name:       "missing snapshot",
			holder:     contextfrag.NewLifecycleHolder(),
			messages:   []runreq.ModelMessage{{Role: "assistant", Content: runreq.NewTextContent("done")}},
			wantReason: "missing_snapshot",
		},
		{
			name:       "missing assistant",
			holder:     lifecycleHolderWithManifest(),
			messages:   []runreq.ModelMessage{{Role: "user", Content: runreq.NewTextContent("hello")}},
			wantReason: "missing_assistant",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			messages := &agenttest.RecordingMessages{}
			service := newTestWriter(Deps{
				Messages: messages,
				Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
					Level: slog.LevelDebug,
				})),
			})

			err := service.StoreRound(context.Background(), runreq.ChatRequest{
				BotID:    "bot-1",
				ThreadID: "session-1",
			}, tt.messages, "model-1", RoundOptions{
				SkipMemory:       true,
				ContextLifecycle: tt.holder,
			})
			if err != nil {
				t.Fatalf("storeRoundWithOptions() error = %v", err)
			}
			if got := logs.String(); !strings.Contains(got, "context lifecycle metadata not persisted") || !strings.Contains(got, tt.wantReason) {
				t.Fatalf("debug log = %q, want reason %q", got, tt.wantReason)
			}
		})
	}
}

func lifecycleHolderWithManifest() *contextfrag.LifecycleHolder {
	holder := contextfrag.NewLifecycleHolder()
	holder.SetManifest(contextfrag.Manifest{View: contextfrag.ViewRunConfigPreProvider})
	return holder
}

func TestFindAssistantMessageForToolCall(t *testing.T) {
	t.Parallel()

	msgs := []messagepkg.Message{
		{ID: "m3", Role: "assistant", Content: []byte(`{"role":"assistant","content":[{"type":"text","text":"done, see call-1 above"}]}`)},
		{ID: "m2", Role: "tool", Content: []byte(`{"role":"tool","content":[{"type":"tool-result","toolCallId":"call-1"}]}`)},
		{ID: "m1", Role: "assistant", Content: []byte(`{"role":"assistant","content":[{"type":"tool-call","toolCallId":"call-1","toolName":"generate_image"}]}`)},
	}

	if got := findAssistantMessageForToolCall(msgs, "call-1"); got != "m1" {
		t.Fatalf("findAssistantMessageForToolCall = %q, want m1 (assistant tool-call row, not tool row or echoed text)", got)
	}
	if got := findAssistantMessageForToolCall(msgs, "call-404"); got != "" {
		t.Fatalf("findAssistantMessageForToolCall unknown id = %q, want empty", got)
	}
}

func TestRepeatedSteerTextDoesNotReuseOriginalTurnIdentity(t *testing.T) {
	service := newTestWriter(Deps{})
	req := runreq.ChatRequest{BotID: storeRoundBotID, ThreadID: "session-1", TurnID: "original-turn", Query: "same", ExternalMessageID: "original-external"}
	inputs, err := service.buildPersistInputs(t.Context(), req, []runreq.ModelMessage{
		{Role: "user", Content: runreq.NewTextContent("same")},
		{Role: "assistant", Content: runreq.NewTextContent("before")},
		{Role: "user", Content: runreq.NewTextContent("same")},
		{Role: "assistant", Content: runreq.NewTextContent("after")},
	}, "", RoundOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if inputs[0].TurnID != "original-turn" || inputs[2].TurnID != "" || inputs[2].ExternalMessageID != "" {
		t.Fatalf("steer reused original identity: %+v", inputs)
	}
}

func TestWorkspaceTargetMetadataNamesTheTarget(t *testing.T) {
	t.Parallel()

	if got := workspaceTargetMetadata(nil); got != nil {
		t.Fatalf("metadata without a target = %#v, want none", got)
	}
	if got := workspaceTargetMetadata(&agentapi.WorkspaceTarget{TargetID: " "}); got != nil {
		t.Fatalf("metadata of a blank target = %#v, want none", got)
	}
	got := workspaceTargetMetadata(&agentapi.WorkspaceTarget{TargetID: " computer-b ", Kind: "remote", Name: "B"})
	location, _ := got["execution_location"].(map[string]any)
	if location["target_id"] != "computer-b" || location["kind"] != "remote" || location["name"] != "B" {
		t.Fatalf("metadata = %#v, want the trimmed target", got)
	}
}

type fullToolResultsSettings struct {
	dbstore.Queries
	full bool
}

func (q fullToolResultsSettings) GetSettingsByBotID(_ context.Context, botID pgtype.UUID) (sqlc.GetSettingsByBotIDRow, error) {
	return sqlc.GetSettingsByBotIDRow{BotID: botID, ReasoningEffort: "medium", PersistFullToolResults: q.full}, nil
}

// A tool result past the payload limit is stored pruned unless the bot keeps
// full tool results.
func TestBuildPersistInputsPrunesToolResultsUnlessBotKeepsThemFull(t *testing.T) {
	t.Parallel()

	unit := "large tool result "
	huge := strings.Repeat(unit, gatewayToolPayloadMaxBytes/len(unit)+2)
	for _, tc := range []struct {
		name       string
		full       bool
		wantPruned bool
	}{
		{"default prunes", false, true},
		{"persist full tool results keeps the original", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			logger := slog.New(slog.DiscardHandler)
			writer := newTestWriter(Deps{
				Logger: logger,
				Bot:    agenttest.BotEnvWithSettings(logger, settings.NewService(logger, fullToolResultsSettings{full: tc.full}, nil, nil)),
			})
			inputs, err := writer.buildPersistInputs(context.Background(), runreq.ChatRequest{
				BotID: storeRoundBotID, ThreadID: "session-1",
			}, []runreq.ModelMessage{{Role: "tool", Content: runreq.NewTextContent(huge)}}, "", RoundOptions{})
			if err != nil {
				t.Fatalf("buildPersistInputs() error = %v", err)
			}
			content := string(inputs[0].Content)
			if pruned := strings.Contains(content, textprune.DefaultMarker); pruned != tc.wantPruned {
				t.Fatalf("pruned = %t, want %t: %.120q", pruned, tc.wantPruned, content)
			}
			if kept := strings.Contains(content, huge); kept == tc.wantPruned {
				t.Fatalf("original kept = %t with wantPruned %t", kept, tc.wantPruned)
			}
		})
	}
}
