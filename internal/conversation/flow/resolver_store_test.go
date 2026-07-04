package flow

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/conversation"
	messagepkg "github.com/memohai/memoh/internal/message"
)

func TestBuildInteractionMetadataIncludesForwardConversation(t *testing.T) {
	t.Parallel()

	meta := buildInteractionMetadata(conversation.ChatRequest{
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

	meta := buildInteractionMetadata(conversation.ChatRequest{
		RequestedSkills: []conversation.RequestedSkillContext{
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
				SourceKind: "plugin",
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
	if raw[1]["name"] != "reviewer" || raw[1]["source_kind"] != "plugin" {
		t.Fatalf("unexpected second skill metadata: %#v", raw[1])
	}
}

func TestBuildInteractionMetadataIncludesPublicSkillActivation(t *testing.T) {
	t.Parallel()

	meta := buildInteractionMetadata(conversation.ChatRequest{
		UserMessageKind: conversation.UserMessageKindSkillActivation,
		SkillActivation: &conversation.SkillActivation{
			Prompt: "do it",
			Skills: []conversation.SkillActivationSkill{{
				Name:        "writer",
				DisplayName: "Writer",
				Description: "short safe description",
				SourceKind:  "managed",
				State:       "effective",
			}},
		},
		RequestedSkills: []conversation.RequestedSkillContext{{
			Name:           "writer",
			SourceKind:     "managed",
			OpaqueSourceID: "opaque-source",
			Content:        "raw content must not leak",
			ContentHash:    "hash-must-not-leak",
			Identity:       "writer|opaque-source|hash",
		}},
	})

	if meta["user_message_kind"] != conversation.UserMessageKindSkillActivation {
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
	recordingMessageService
	batchInputs []messagepkg.PersistInput
}

func (s *batchRecordingMessageService) PersistToolTailRound(_ context.Context, inputs []messagepkg.PersistInput) ([]messagepkg.Message, bool, error) {
	s.batchInputs = append(s.batchInputs, inputs...)
	return recordedMessages(inputs), true, nil
}

func TestStoreMessagesUsesToolTailBatch(t *testing.T) {
	t.Parallel()

	messages := &batchRecordingMessageService{}
	resolver := &Resolver{
		messageService: messages,
		logger:         slog.New(slog.DiscardHandler),
	}

	persisted := resolver.storeMessages(context.Background(), conversation.ChatRequest{
		BotID:       storeRoundBotID,
		SessionID:   "33333333-3333-3333-3333-333333333333",
		Query:       "hello",
		SessionType: "chat",
		RuntimeType: "model",
	}, []conversation.ModelMessage{
		{Role: "user", Content: conversation.NewTextContent("hello")},
		{Role: "assistant", Content: conversation.NewTextContent("call tool")},
		{Role: "tool", Content: conversation.NewTextContent("tool result")},
		{Role: "assistant", Content: conversation.NewTextContent("done")},
	}, "", storeRoundOptions{})

	if len(messages.batchInputs) != 4 {
		t.Fatalf("batch inputs = %d, want 4", len(messages.batchInputs))
	}
	if len(messages.persisted) != 0 {
		t.Fatalf("fallback Persist called %d times, want 0", len(messages.persisted))
	}
	if len(persisted) != 4 {
		t.Fatalf("persisted messages = %d, want 4", len(persisted))
	}
}

func TestStoreRoundPersistsContextLifecycleMetadataOnAssistant(t *testing.T) {
	t.Parallel()

	ledger := contextfrag.NewMutationLedger()
	ledger.Record(contextfrag.MutationMidTaskPrune, "pruned=2")
	ledger.SetFinalInputHash("final-hash")
	holder := contextfrag.NewLifecycleHolder()
	holder.SetManifest(contextfrag.Manifest{
		View: contextfrag.ViewRunConfigPreProvider,
		Counts: contextfrag.ManifestCounts{
			Fragments: 2,
			Messages:  1,
			TextBytes: 64,
		},
		Selection: &contextfrag.SelectionTrace{
			Selected:    1,
			Dropped:     1,
			DropReasons: map[string]int{"budget_trim": 1},
		},
		CachePlan: &contextfrag.CachePlan{StablePrefixHash: "prefix-hash", StableMessageCount: 1},
		Mutations: ledger,
	})
	messages := &recordingMessageService{}
	resolver := &Resolver{
		logger:         slog.Default(),
		messageService: messages,
	}

	err := resolver.storeRoundWithOptions(context.Background(), conversation.ChatRequest{
		BotID:     "bot-1",
		SessionID: "session-1",
		Query:     "hello",
	}, []conversation.ModelMessage{
		{Role: "user", Content: conversation.NewTextContent("hello")},
		{Role: "assistant", Content: conversation.NewTextContent("hi")},
	}, "model-1", storeRoundOptions{ContextLifecycle: holder})
	if err != nil {
		t.Fatalf("storeRoundWithOptions error: %v", err)
	}
	if len(messages.persisted) != 2 {
		t.Fatalf("persisted messages = %d, want 2", len(messages.persisted))
	}
	assistantMeta := messages.persisted[1].Metadata
	raw, err := json.Marshal(assistantMeta[contextfrag.MetadataContextLifecycleKey])
	if err != nil {
		t.Fatalf("marshal lifecycle metadata: %v", err)
	}
	for _, want := range []string{"run_config_pre_provider", "prefix-hash", "final-hash", "budget_trim"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("lifecycle metadata missing %q: %s", want, raw)
		}
	}
	if strings.Contains(string(raw), `"items"`) {
		t.Fatalf("lifecycle metadata must use condensed snapshot: %s", raw)
	}
}

func TestStoreRoundLogsWhenContextLifecycleMissing(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	messages := &recordingMessageService{}
	resolver := &Resolver{
		logger:         logger,
		messageService: messages,
	}

	err := resolver.storeRoundWithOptions(context.Background(), conversation.ChatRequest{
		BotID:     "bot-acp",
		SessionID: "session-acp",
		Query:     "hello",
	}, []conversation.ModelMessage{
		{Role: "user", Content: conversation.NewTextContent("hello")},
		{Role: "assistant", Content: conversation.NewTextContent("hi")},
	}, "model-1", storeRoundOptions{})
	if err != nil {
		t.Fatalf("storeRoundWithOptions error: %v", err)
	}
	if got := logs.String(); !strings.Contains(got, "context lifecycle metadata not persisted") || !strings.Contains(got, "missing_lifecycle") {
		t.Fatalf("debug log = %q, want lifecycle metadata skip reason", got)
	}
	if len(messages.persisted) != 2 {
		t.Fatalf("persisted messages = %d, want 2", len(messages.persisted))
	}
	if _, ok := messages.persisted[1].Metadata[contextfrag.MetadataContextLifecycleKey]; ok {
		t.Fatalf("unexpected lifecycle metadata: %#v", messages.persisted[1].Metadata)
	}
}
