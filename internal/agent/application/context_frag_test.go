package application

import (
	"context"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/agent/runtime/native"
	"github.com/memohai/memoh/internal/agent/turn"
	"github.com/memohai/memoh/internal/contextview"
)

type fakePlatformIdentitySource struct {
	identities []PlatformIdentity
}

func (f fakePlatformIdentitySource) ListPlatformIdentities(context.Context, string) ([]PlatformIdentity, error) {
	return f.identities, nil
}

func TestBuildContextFragScopePreservesIMTopology(t *testing.T) {
	t.Parallel()

	scope := buildContextFragScope(ChatRequest{
		BotID:                     "bot-1",
		ChatID:                    "chat-1",
		ThreadID:                  "sess-1",
		SourceChannelIdentityID:   "identity-1",
		DisplayName:               "ignored",
		CurrentChannel:            "telegram",
		ConversationType:          turn.ConversationTypeGroup,
		ConversationName:          "Research Group",
		ReplyTarget:               "group-1",
		ExternalMessageID:         "msg-1",
		EventID:                   "evt-1",
		SourceReplyToMessageID:    "msg-0",
		ReplySender:               "Alice",
		MentionsBot:               true,
		RepliesToBot:              true,
		ForwardMessageID:          "fwd-1",
		ForwardFromUserID:         "user-2",
		ForwardFromConversationID: "source-chat",
		RawQuery:                  "/summarize this",
	}, "Bob", native.SessionContext{})

	if scope.BotID != "bot-1" || scope.ChatID != "chat-1" || scope.SessionID != "sess-1" {
		t.Fatalf("unexpected base scope: %#v", scope)
	}
	if scope.Platform != "telegram" || scope.ConversationType != turn.ConversationTypeGroup || scope.ConversationName != "Research Group" {
		t.Fatalf("unexpected conversation scope: %#v", scope)
	}
	if scope.CurrentMessageID != "msg-1" || scope.EventID != "evt-1" || scope.ReplyToMessageID != "msg-0" {
		t.Fatalf("unexpected message topology: %#v", scope)
	}
	if !scope.MentionsBot || !scope.RepliesToBot {
		t.Fatalf("expected structured directed-at-bot flags in scope: %#v", scope)
	}
	if scope.ForwardMessageID != "fwd-1" || scope.ForwardFromUserID != "user-2" || scope.ForwardFromConversationID != "source-chat" {
		t.Fatalf("unexpected forward topology: %#v", scope)
	}
	if !hasAttention(scope.Attention, contextfrag.AttentionReply) || !hasAttention(scope.Attention, contextfrag.AttentionCommand) {
		t.Fatalf("attention reasons = %#v, want reply and command", scope.Attention)
	}
	if !hasAttention(scope.Attention, contextfrag.AttentionMention) {
		t.Fatalf("attention reasons = %#v, want mention", scope.Attention)
	}
	if hasAttention(scope.Attention, contextfrag.AttentionPassive) {
		t.Fatalf("attention reasons should not include passive when reply/command are present: %#v", scope.Attention)
	}
}

func TestBuildContextFragScopeDoesNotInferDirectedReplyFromAnyReplyID(t *testing.T) {
	t.Parallel()

	scope := buildContextFragScope(ChatRequest{
		BotID:                  "bot-1",
		ChatID:                 "chat-1",
		ThreadID:               "sess-1",
		ConversationType:       turn.ConversationTypeGroup,
		SourceReplyToMessageID: "someone-elses-message",
		Query:                  "thread side comment",
	}, "Bob", native.SessionContext{})

	if scope.ReplyToMessageID != "someone-elses-message" {
		t.Fatalf("reply topology not preserved: %#v", scope)
	}
	if hasAttention(scope.Attention, contextfrag.AttentionReply) || hasAttention(scope.Attention, contextfrag.AttentionMention) {
		t.Fatalf("attention should not infer directed reply/mention without structured flags: %#v", scope.Attention)
	}
	if !hasAttention(scope.Attention, contextfrag.AttentionPassive) {
		t.Fatalf("group reply without directed flags should be passive attention: %#v", scope.Attention)
	}
}

func TestPrepareRunConfigDoesNotDoubleCountPipelineInlineImages(t *testing.T) {
	t.Parallel()

	image := sdk.ImagePart{Image: "data:image/png;base64,abc", MediaType: "image/png"}
	resolver := &Service{}
	cfg := native.RunConfig{
		Messages:     []sdk.Message{sdk.UserMessage("pipeline current user")},
		InlineImages: []sdk.ImagePart{image},
	}

	got := resolver.prepareRunConfig(context.Background(), cfg)
	got = applyProviderRunConfigForTest(t, got)

	if got.ContextManifest.Counts.Images != 1 {
		t.Fatalf("manifest image count = %d, want the image counted exactly once: %#v", got.ContextManifest.Counts.Images, got.ContextManifest.Items)
	}
	if !messagesContainImage(got.Messages) {
		t.Fatalf("prepared messages do not contain injected image: %#v", got.Messages)
	}
	if countMessagesWithImage(got.Messages) != 1 {
		t.Fatalf("image must land in exactly one message: %#v", got.Messages)
	}
	if !got.ContextQueryMaterialized {
		t.Fatal("view must mark the query materialized after rendering")
	}
	if len(got.ContextSourceFrags) == 0 {
		t.Fatal("prepareRunConfig must collect first-class source fragments")
	}
}

// TestPrepareRunConfigClearsStaleContextHookText proves the resume
// double-call scenario stays correct: session-continuation resume calls
// ResolveRunConfig (which calls prepareRunConfig once internally) and then
// calls prepareRunConfig a second time on the resulting RunConfig. If this
// second call's hooks produce no text (as here, with hookService nil so
// hooks never fire), any ContextHookText left over from the earlier call
// must not survive into the result.
func TestPrepareRunConfigClearsStaleContextHookText(t *testing.T) {
	t.Parallel()

	resolver := &Service{}
	cfg := native.RunConfig{
		Identity:        native.SessionContext{BotID: "bot-1"},
		ContextHookText: "[Hook Context: BeforePromptBuild]\nstale text from a prior turn",
	}

	got := resolver.prepareRunConfig(context.Background(), cfg)

	if got.ContextHookText != "" {
		t.Fatalf("ContextHookText = %q, want empty (stale hook text from an earlier prepareRunConfig call must not survive a call whose hooks produce no text)", got.ContextHookText)
	}
}

func TestPrepareRunConfigSeparatesMemoryRecallFromMemoryHookContext(t *testing.T) {
	t.Parallel()

	resolver := &Service{}
	cfg := native.RunConfig{
		Identity:              native.SessionContext{BotID: "bot-1"},
		ContextMemoryText:     "remembered fact",
		ContextMemoryHookText: "[Hook Context: AfterMemorySearch]\nplugin guidance",
	}

	got := resolver.prepareRunConfig(context.Background(), cfg)
	if got.ContextMemoryHookText != "" {
		t.Fatalf("one-shot memory hook carrier survived prepare: %q", got.ContextMemoryHookText)
	}
	if got.ContextHookText != cfg.ContextMemoryHookText {
		t.Fatalf("hook context = %q, want separately carried memory hook", got.ContextHookText)
	}

	var memoryText, hookText string
	for _, frag := range got.ContextSourceFrags {
		switch frag.Kind {
		case contextfrag.KindMemoryRecall:
			memoryText = contextFragText(frag)
		case contextfrag.KindHookContext:
			hookText = contextFragText(frag)
		}
	}
	if !strings.Contains(memoryText, "remembered fact") || strings.Contains(memoryText, "plugin guidance") {
		t.Fatalf("memory fragment = %q, want provider recall only", memoryText)
	}
	if !strings.Contains(hookText, "plugin guidance") {
		t.Fatalf("hook fragment = %q, want hook output", hookText)
	}
}

func contextFragText(frag contextfrag.ContextFrag) string {
	for _, part := range frag.Parts {
		if part.SDKMessage == nil {
			continue
		}
		for _, messagePart := range part.SDKMessage.Content {
			if text, ok := messagePart.(sdk.TextPart); ok {
				return text.Text
			}
		}
	}
	return ""
}

func TestPrepareRunConfigSourceFragsCarryTypedSystemKinds(t *testing.T) {
	t.Parallel()

	resolver := &Service{}
	resolver.SetPlatformIdentitySource(fakePlatformIdentitySource{identities: []PlatformIdentity{
		{Platform: "telegram", ExternalIdentity: "12345"},
	}})
	cfg := native.RunConfig{
		Identity: native.SessionContext{BotID: "bot-1"},
		Bot:      native.BotInfo{ID: "bot-1", Name: "research-bot"},
		Messages: []sdk.Message{sdk.UserMessage("hi")},
	}

	got := resolver.prepareRunConfig(context.Background(), cfg)

	kinds := make(map[contextfrag.Kind]bool, len(got.ContextSourceFrags))
	for _, frag := range got.ContextSourceFrags {
		kinds[frag.Kind] = true
	}
	if !kinds[contextfrag.KindBotIdentity] {
		t.Fatalf("ContextSourceFrags missing KindBotIdentity: %#v", got.ContextSourceFrags)
	}
	if !kinds[contextfrag.KindPlatformIdentity] {
		t.Fatalf("ContextSourceFrags missing KindPlatformIdentity: %#v", got.ContextSourceFrags)
	}
}

// TestPrepareRunConfigFragsFirstMatchesLegacyReverseParse is the byte-equivalence
// gate: prepareRunConfig now builds ContextSourceFrags directly from
// GenerateSystemSections instead of letting CollectProviderSourceFrags
// reverse-parse cfg.System. Both must still render to the identical provider
// System string and message stream, including when a ContextToolUsage
// fragment is spliced in afterward by ApplyProviderRunConfig.
func TestPrepareRunConfigFragsFirstMatchesLegacyReverseParse(t *testing.T) {
	t.Parallel()

	baseCfg := native.RunConfig{
		Identity:     native.SessionContext{BotID: "bot-1"},
		Bot:          native.BotInfo{ID: "bot-1", Name: "research-bot", DisplayName: "Research Bot"},
		Skills:       []native.SkillEntry{{Name: "foo-skill", Description: "does foo things"}},
		Messages:     []sdk.Message{sdk.UserMessage("earlier question"), sdk.AssistantMessage("earlier answer")},
		Query:        "current question",
		InlineImages: []sdk.ImagePart{{Image: "data:image/png;base64,abc", MediaType: "image/png"}},
	}
	resolver := &Service{}

	got1 := resolver.prepareRunConfig(context.Background(), baseCfg)
	got1.ContextToolUsage = "## Tool usage\n\nUSE_TOOLS"
	rendered1 := applyProviderRunConfigForTest(t, got1)

	got2 := resolver.prepareRunConfig(context.Background(), baseCfg)
	got2.ContextSourceFrags = contextview.CollectProviderSourceFrags(context.Background(), got2)
	got2.ContextToolUsage = "## Tool usage\n\nUSE_TOOLS"
	rendered2 := applyProviderRunConfigForTest(t, got2)

	if rendered1.System != rendered2.System {
		t.Fatalf("frags-first System diverges from legacy reverse-parse System:\ngot:  %q\nwant: %q", rendered1.System, rendered2.System)
	}
	if !reflect.DeepEqual(rendered1.Messages, rendered2.Messages) {
		t.Fatalf("messages diverge:\ngot:  %#v\nwant: %#v", rendered1.Messages, rendered2.Messages)
	}
}

func hasAttention(reasons []contextfrag.AttentionReason, want contextfrag.AttentionReason) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}

func messagesContainImage(messages []sdk.Message) bool {
	for _, message := range messages {
		for _, part := range message.Content {
			if _, ok := part.(sdk.ImagePart); ok {
				return true
			}
		}
	}
	return false
}

func countMessagesWithImage(messages []sdk.Message) int {
	count := 0
	for _, msg := range messages {
		for _, part := range msg.Content {
			if _, ok := part.(sdk.ImagePart); ok {
				count++
				break
			}
		}
	}
	return count
}
