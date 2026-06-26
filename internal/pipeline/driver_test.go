package pipeline

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
	messagepkg "github.com/memohai/memoh/internal/message"
)

func TestExtractNewImageRefs(t *testing.T) {
	rc := RenderedContext{
		{ReceivedAtMs: 100, ImageRefs: []ImageAttachmentRef{{ContentHash: "old-hash", Mime: "image/png"}}},
		{ReceivedAtMs: 200, IsMyself: true, ImageRefs: []ImageAttachmentRef{{ContentHash: "self-hash"}}},
		{ReceivedAtMs: 300, ImageRefs: []ImageAttachmentRef{{ContentHash: "new-hash", Mime: "image/jpeg"}}},
		{ReceivedAtMs: 400, ImageRefs: nil},
	}

	refs := extractNewImageRefs(rc, 150)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	if refs[0].ContentHash != "new-hash" {
		t.Fatalf("expected new-hash, got %q", refs[0].ContentHash)
	}
	if refs[0].Mime != "image/jpeg" {
		t.Fatalf("expected image/jpeg, got %q", refs[0].Mime)
	}
}

func TestExtractNewImageRefs_IncludesMultiple(t *testing.T) {
	rc := RenderedContext{
		{ReceivedAtMs: 100},
		{ReceivedAtMs: 200, ImageRefs: []ImageAttachmentRef{
			{ContentHash: "a"},
			{ContentHash: "b"},
		}},
		{ReceivedAtMs: 300, ImageRefs: []ImageAttachmentRef{{ContentHash: "c"}}},
	}
	refs := extractNewImageRefs(rc, 50)
	if len(refs) != 3 {
		t.Fatalf("expected 3 refs, got %d", len(refs))
	}
}

func TestInjectImagePartsIntoLastUserMessage(t *testing.T) {
	msgs := []sdk.Message{
		sdk.UserMessage("hello"),
		sdk.AssistantMessage("hi"),
		sdk.UserMessage("look at this"),
	}
	parts := []sdk.ImagePart{
		{Image: "data:image/png;base64,abc", MediaType: "image/png"},
	}

	injectImagePartsIntoLastUserMessage(msgs, parts)

	lastUser := msgs[2]
	if len(lastUser.Content) != 2 {
		t.Fatalf("expected 2 content parts, got %d", len(lastUser.Content))
	}
	imgPart, ok := lastUser.Content[1].(sdk.ImagePart)
	if !ok {
		t.Fatalf("expected ImagePart, got %T", lastUser.Content[1])
	}
	if imgPart.Image != "data:image/png;base64,abc" {
		t.Fatalf("unexpected image: %q", imgPart.Image)
	}
}

func TestInjectImagePartsIntoLastUserMessage_Empty(t *testing.T) {
	msgs := []sdk.Message{sdk.UserMessage("hello")}
	injectImagePartsIntoLastUserMessage(msgs, nil)
	if len(msgs[0].Content) != 1 {
		t.Fatalf("expected no change, got %d parts", len(msgs[0].Content))
	}
}

func TestInjectImagePartsIntoLastUserMessage_SkipsEmptyImage(t *testing.T) {
	msgs := []sdk.Message{sdk.UserMessage("hello")}
	parts := []sdk.ImagePart{{Image: "", MediaType: "image/png"}}
	injectImagePartsIntoLastUserMessage(msgs, parts)
	if len(msgs[0].Content) != 1 {
		t.Fatalf("expected no change, got %d parts", len(msgs[0].Content))
	}
}

func TestHandleReplyWithAgent_InlinesImages(t *testing.T) {
	rc := RenderedContext{
		{
			ReceivedAtMs: 200,
			Content:      []RenderedContentPiece{{Type: "text", Text: `<message id="1">photo</message>`}},
			ImageRefs:    []ImageAttachmentRef{{ContentHash: "img-hash", Mime: "image/jpeg"}},
		},
	}

	fakeAgent := &fakeDiscussStreamer{}

	resolver := &fakeRunConfigResolver{
		resolveResult: ResolveRunConfigResult{
			RunConfig: agentpkg.RunConfig{
				SupportsImageInput: true,
			},
			ModelID: "model-1",
		},
		inlineFn: func(_ context.Context, _ string, refs []ImageAttachmentRef) []sdk.ImagePart {
			if len(refs) != 1 || refs[0].ContentHash != "img-hash" {
				t.Fatalf("unexpected refs: %v", refs)
			}
			return []sdk.ImagePart{{Image: "data:image/jpeg;base64,FAKE", MediaType: "image/jpeg"}}
		},
	}

	driver := NewDiscussDriver(DiscussDriverDeps{
		Pipeline: NewPipeline(RenderParams{}),
		Resolver: resolver,
	})

	sess := &discussSession{
		config: DiscussSessionConfig{
			BotID:     "bot-1",
			SessionID: "sess-1",
		},
		lastProcessedMs: 0,
	}

	driver.handleReplyWithAgent(context.Background(), sess, rc, driver.logger, fakeAgent)

	if fakeAgent.lastConfig == nil {
		t.Fatal("expected agent to be called")
	}

	msgs := fakeAgent.lastConfig.Messages
	var userMsgs []sdk.Message
	for _, m := range msgs {
		if m.Role == sdk.MessageRoleUser {
			userMsgs = append(userMsgs, m)
		}
	}
	if len(userMsgs) < 2 {
		t.Fatalf("expected at least 2 user messages (rc + late binding), got %d", len(userMsgs))
	}
	rcMsg := userMsgs[0]
	hasImage := false
	for _, part := range rcMsg.Content {
		if imgPart, ok := part.(sdk.ImagePart); ok {
			hasImage = true
			if !strings.HasPrefix(imgPart.Image, "data:image/jpeg;base64,") {
				t.Fatalf("unexpected image data: %q", imgPart.Image)
			}
		}
	}
	if !hasImage {
		t.Fatal("expected image part in RC user message")
	}
}

func TestHandleReplyWithAgent_NoInlineWhenNoVision(t *testing.T) {
	rc := RenderedContext{
		{
			ReceivedAtMs: 200,
			Content:      []RenderedContentPiece{{Type: "text", Text: `<message id="1">photo</message>`}},
			ImageRefs:    []ImageAttachmentRef{{ContentHash: "img-hash", Mime: "image/jpeg"}},
		},
	}

	fakeAgent := &fakeDiscussStreamer{}

	resolver := &fakeRunConfigResolver{
		resolveResult: ResolveRunConfigResult{
			RunConfig: agentpkg.RunConfig{
				SupportsImageInput: false,
			},
			ModelID: "model-1",
		},
		inlineFn: func(_ context.Context, _ string, _ []ImageAttachmentRef) []sdk.ImagePart {
			t.Fatal("should not be called when model doesn't support vision")
			return nil
		},
	}

	driver := NewDiscussDriver(DiscussDriverDeps{
		Pipeline: NewPipeline(RenderParams{}),
		Resolver: resolver,
	})

	sess := &discussSession{
		config: DiscussSessionConfig{
			BotID:     "bot-1",
			SessionID: "sess-1",
		},
		lastProcessedMs: 0,
	}

	driver.handleReplyWithAgent(context.Background(), sess, rc, driver.logger, fakeAgent)

	if fakeAgent.lastConfig == nil {
		t.Fatal("expected agent to be called")
	}
	for _, m := range fakeAgent.lastConfig.Messages {
		for _, part := range m.Content {
			if _, ok := part.(sdk.ImagePart); ok {
				t.Fatal("should not have image parts when vision is not supported")
			}
		}
	}
}

func TestAnchorFromTRs(t *testing.T) {
	t.Parallel()

	if got := anchorFromTRs(nil); got != 0 {
		t.Fatalf("empty TRs anchor = %d, want 0", got)
	}
	got := anchorFromTRs([]TurnResponseEntry{
		{RequestedAtMs: 100},
		{RequestedAtMs: 500},
		{RequestedAtMs: 300},
	})
	if got != 500 {
		t.Fatalf("anchor = %d, want 500", got)
	}
}

func TestLatestRCReceivedAtMs(t *testing.T) {
	t.Parallel()

	if got := latestRCReceivedAtMs(nil); got != 0 {
		t.Fatalf("empty RC = %d, want 0", got)
	}
	got := latestRCReceivedAtMs(RenderedContext{
		{ReceivedAtMs: 100},
		{ReceivedAtMs: 900},
		{ReceivedAtMs: 500, IsMyself: true},
	})
	if got != 900 {
		t.Fatalf("latest = %d, want 900", got)
	}
}

// TestHandleReplyWithAgent_ColdStartAnchoredByTR simulates idle-timeout
// restart: the session's in-memory lastProcessedMs is 0, but RC replay has
// brought back old user messages that were already answered in prior
// LLM rounds (represented by TRs). The driver MUST NOT re-answer them.
func TestHandleReplyWithAgent_ColdStartAnchoredByTR(t *testing.T) {
	rc := RenderedContext{
		{
			ReceivedAtMs: 100,
			Content:      []RenderedContentPiece{{Type: "text", Text: `<message id="old">task 1</message>`}},
		},
	}

	fakeAgent := &fakeDiscussStreamer{}
	resolver := &fakeRunConfigResolver{}

	driver := NewDiscussDriver(DiscussDriverDeps{
		Pipeline:       NewPipeline(RenderParams{}),
		Resolver:       resolver,
		MessageService: nil,
	})

	sess := &discussSession{
		config:          DiscussSessionConfig{BotID: "b", SessionID: "s"},
		lastProcessedMs: 0,
	}

	// Simulate a previously answered round by pre-stuffing a TR newer than
	// the RC segment's ReceivedAtMs. Since we cannot inject MessageService
	// easily, we instead pre-set lastProcessedMs as the anchor would.
	sess.lastProcessedMs = 200 // mimic anchorFromTRs result

	driver.handleReplyWithAgent(context.Background(), sess, rc, driver.logger, fakeAgent)

	if fakeAgent.lastConfig != nil {
		t.Fatal("agent must not be invoked when all RC segments predate lastProcessedMs")
	}
}

// TestHandleReplyWithAgent_CursorAdvancesToRCNotWallClock ensures that after
// a turn we set lastProcessedMs to the max ReceivedAtMs actually consumed in
// the RC snapshot, not time.Now(). This matters for messages that arrive
// mid-turn: they end up in a fresher RC with ReceivedAtMs > cursor, which
// correctly triggers the next round.
func TestHandleReplyWithAgent_CursorAdvancesToRCNotWallClock(t *testing.T) {
	rc := RenderedContext{
		{
			ReceivedAtMs: 777,
			Content:      []RenderedContentPiece{{Type: "text", Text: `<message id="x">hello</message>`}},
		},
	}
	fakeAgent := &fakeDiscussStreamer{}
	resolver := &fakeRunConfigResolver{}
	driver := NewDiscussDriver(DiscussDriverDeps{
		Pipeline: NewPipeline(RenderParams{}),
		Resolver: resolver,
	})
	sess := &discussSession{
		config:          DiscussSessionConfig{BotID: "b", SessionID: "s"},
		lastProcessedMs: 0,
	}

	driver.handleReplyWithAgent(context.Background(), sess, rc, driver.logger, fakeAgent)

	if fakeAgent.lastConfig == nil {
		t.Fatal("expected agent to be invoked")
	}
	if sess.lastProcessedMs != 777 {
		t.Fatalf("lastProcessedMs = %d, want 777 (max RC ReceivedAtMs)", sess.lastProcessedMs)
	}
}

func TestHandleReplyWithAgentRefreshesContextFragAfterLateBinding(t *testing.T) {
	rc := RenderedContext{
		{
			ReceivedAtMs: 200,
			Content:      []RenderedContentPiece{{Type: "text", Text: `<message id="x">hello</message>`}},
		},
	}
	fakeAgent := &fakeDiscussStreamer{}
	resolver := &fakeRunConfigResolver{
		resolveResult: ResolveRunConfigResult{
			RunConfig: agentpkg.RunConfig{System: "base system"},
			ModelID:   "model-1",
		},
	}
	driver := NewDiscussDriver(DiscussDriverDeps{
		Pipeline: NewPipeline(RenderParams{}),
		Resolver: resolver,
	})
	sess := &discussSession{
		config:          DiscussSessionConfig{BotID: "b", SessionID: "s"},
		lastProcessedMs: 0,
	}

	driver.handleReplyWithAgent(context.Background(), sess, rc, driver.logger, fakeAgent)

	if fakeAgent.lastConfig == nil {
		t.Fatal("expected agent to be invoked")
	}
	cfg := fakeAgent.lastConfig
	if cfg.ContextManifest.Counts.Messages != len(cfg.Messages) {
		t.Fatalf("manifest message count = %d, messages = %d", cfg.ContextManifest.Counts.Messages, len(cfg.Messages))
	}
	if !lastMessageFragContains(cfg.ContextFrags, "IMPORTANT: You MUST use the `send` tool") {
		t.Fatalf("context frags do not include late-binding prompt: %#v", cfg.ContextManifest.Items)
	}
}

func TestHandleReplyWithAgentUsesCompactionSummary(t *testing.T) {
	rc := RenderedContext{
		{
			MessageID:    "external-old",
			ReceivedAtMs: 100,
			Content:      []RenderedContentPiece{{Type: "text", Text: `<message id="external-old">old</message>`}},
		},
		{
			MessageID:    "external-new",
			ReceivedAtMs: 300,
			Content:      []RenderedContentPiece{{Type: "text", Text: `<message id="external-new">new</message>`}},
		},
	}
	fakeAgent := &fakeDiscussStreamer{}
	resolver := &fakeRunConfigResolver{
		resolveResult: ResolveRunConfigResult{
			RunConfig: agentpkg.RunConfig{System: "base system"},
			ModelID:   "model-1",
		},
		compactionSummary: CompactSummary{
			Text:              "old summarized",
			CoveredMessageIDs: []string{"external-old"},
		},
	}
	driver := NewDiscussDriver(DiscussDriverDeps{
		Pipeline: NewPipeline(RenderParams{}),
		Resolver: resolver,
	})
	sess := &discussSession{
		config:          DiscussSessionConfig{BotID: "b", SessionID: "s"},
		lastProcessedMs: 0,
	}

	driver.handleReplyWithAgent(context.Background(), sess, rc, driver.logger, fakeAgent)

	if fakeAgent.lastConfig == nil {
		t.Fatal("expected agent to be invoked")
	}
	got := sdkMessageTexts(fakeAgent.lastConfig.Messages)
	if len(got) < 2 {
		t.Fatalf("expected summary and current message, got %#v", got)
	}
	if got[0] != "[Conversation summary]\nold summarized" {
		t.Fatalf("first message = %q, want compaction summary; all=%#v", got[0], got)
	}
	if strings.Contains(strings.Join(got, "\n"), "external-old") {
		t.Fatalf("covered RC replay was not skipped: %#v", got)
	}
	if !strings.Contains(strings.Join(got, "\n"), "external-new") {
		t.Fatalf("uncovered RC replay missing: %#v", got)
	}
}

// --- Test helpers ---

type fakeDiscussStreamer struct {
	lastConfig *agentpkg.RunConfig
}

func (f *fakeDiscussStreamer) Stream(_ context.Context, cfg agentpkg.RunConfig) <-chan agentpkg.StreamEvent {
	f.lastConfig = &cfg
	ch := make(chan agentpkg.StreamEvent, 1)
	ch <- agentpkg.StreamEvent{Type: agentpkg.EventAgentEnd}
	close(ch)
	return ch
}

type fakeRunConfigResolver struct {
	resolveResult     ResolveRunConfigResult
	inlineFn          func(ctx context.Context, botID string, refs []ImageAttachmentRef) []sdk.ImagePart
	compactionSummary CompactSummary
}

func (f *fakeRunConfigResolver) ResolveRunConfig(_ context.Context, _, _, _, _, _, _, _ string) (ResolveRunConfigResult, error) {
	return f.resolveResult, nil
}

func (f *fakeRunConfigResolver) InlineImageAttachments(ctx context.Context, botID string, refs []ImageAttachmentRef) []sdk.ImagePart {
	if f.inlineFn != nil {
		return f.inlineFn(ctx, botID, refs)
	}
	return nil
}

func (*fakeRunConfigResolver) StoreRound(_ context.Context, _, _, _, _ string, _ []sdk.Message, _ string) error {
	return nil
}

func (f *fakeRunConfigResolver) LoadCompactionSummary(context.Context, []messagepkg.Message) CompactSummary {
	return f.compactionSummary
}

func sdkMessageTexts(messages []sdk.Message) []string {
	out := make([]string, 0, len(messages))
	for _, msg := range messages {
		var parts []string
		for _, part := range msg.Content {
			if text, ok := part.(sdk.TextPart); ok {
				parts = append(parts, text.Text)
			}
		}
		out = append(out, strings.Join(parts, "\n"))
	}
	return out
}

func lastMessageFragContains(frags []contextfrag.ContextFrag, needle string) bool {
	for i := len(frags) - 1; i >= 0; i-- {
		frag := frags[i]
		if frag.Kind != contextfrag.KindConversationEvent || len(frag.Parts) == 0 || frag.Parts[0].SDKMessage == nil {
			continue
		}
		for _, part := range frag.Parts[0].SDKMessage.Content {
			if text, ok := part.(sdk.TextPart); ok && strings.Contains(text.Text, needle) {
				return true
			}
		}
		return false
	}
	return false
}
