package discuss

import (
	"context"
	"strings"
	"testing"

	"github.com/memohai/memoh/internal/chat/timeline"
)

type fakeArtifactProvider struct {
	artifacts []timeline.CompactionArtifact
	botID     string
	sessionID string
}

func (f *fakeArtifactProvider) ActiveCompactionArtifacts(_ context.Context, botID, sessionID string) ([]timeline.CompactionArtifact, error) {
	f.botID = botID
	f.sessionID = sessionID
	return f.artifacts, nil
}

func TestHandleReplyWithTurn_InsertsArtifactSummary(t *testing.T) {
	rc := timeline.RenderedContext{
		{
			MessageID:    "m1",
			ReceivedAtMs: 100,
			Content:      []timeline.RenderedContentPiece{{Type: "text", Text: `<message id="m1">old original</message>`}},
		},
		{
			MessageID:    "m2",
			ReceivedAtMs: 200,
			Content:      []timeline.RenderedContentPiece{{Type: "text", Text: `<message id="m2">current question</message>`}},
		},
	}
	artifacts := []timeline.CompactionArtifact{{
		ID:            "a1",
		Summary:       "compacted window",
		AnchorStartMs: 100,
		Sources:       []timeline.CompactionSource{{ExternalMessageID: "m1", CreatedAtMs: 100}},
	}}
	provider := &fakeArtifactProvider{artifacts: artifacts}
	svc := &fakeTurnService{}
	driver := NewDiscussDriver(DiscussDriverDeps{Artifacts: provider})
	sess := &discussSession{
		config: DiscussSessionConfig{TeamID: "team-1", BotID: "bot-1", ThreadID: "sess-1"},
	}

	driver.handleReplyWithTurn(context.Background(), sess, rc, driver.logger, svc)

	if svc.calls != 1 {
		t.Fatalf("StartTurn calls = %d, want 1", svc.calls)
	}
	if provider.botID != "bot-1" || provider.sessionID != "sess-1" {
		t.Fatalf("artifact provider scoped to %q/%q", provider.botID, provider.sessionID)
	}

	cmd := svc.lastCmd
	expected := timeline.ComposeContextWithArtifacts(rc, nil, artifacts)
	if expected == nil || len(cmd.DiscussMessages) != len(expected.Messages) {
		t.Fatalf("discuss messages diverge from shared composition: got %d, want %d", len(cmd.DiscussMessages), len(expected.Messages))
	}
	for i, message := range expected.Messages {
		if cmd.DiscussMessages[i].Role != message.Role || cmd.DiscussMessages[i].Content != message.Content {
			t.Fatalf("message %d diverges: %+v vs %+v", i, cmd.DiscussMessages[i], message)
		}
	}
	if !strings.Contains(cmd.DiscussMessages[0].Content, "<summary>") || !strings.Contains(cmd.DiscussMessages[0].Content, "compacted window") {
		t.Fatalf("expected leading summary, got %+v", cmd.DiscussMessages[0])
	}
	var joinedParts []string
	for _, message := range cmd.DiscussMessages {
		joinedParts = append(joinedParts, message.Content)
	}
	joined := strings.Join(joinedParts, "|")
	if strings.Contains(joined, "old original") {
		t.Fatalf("covered original must be replaced, got %s", joined)
	}
	if !strings.Contains(joined, "current question") {
		t.Fatalf("uncovered message must survive, got %s", joined)
	}
}

func TestHandleReplyWithTurn_NilArtifactProviderComposesPlain(t *testing.T) {
	rc := timeline.RenderedContext{
		{
			MessageID:    "m1",
			ReceivedAtMs: 200,
			Content:      []timeline.RenderedContentPiece{{Type: "text", Text: `<message id="m1">hello</message>`}},
		},
	}
	svc := &fakeTurnService{}
	driver := NewDiscussDriver(DiscussDriverDeps{})
	sess := &discussSession{config: DiscussSessionConfig{BotID: "bot-1", ThreadID: "sess-1"}}

	driver.handleReplyWithTurn(context.Background(), sess, rc, driver.logger, svc)

	if svc.calls != 1 {
		t.Fatalf("StartTurn calls = %d, want 1", svc.calls)
	}
	if len(svc.lastCmd.DiscussMessages) != 1 || !strings.Contains(svc.lastCmd.DiscussMessages[0].Content, "hello") {
		t.Fatalf("expected plain composition, got %+v", svc.lastCmd.DiscussMessages)
	}
}

func TestWasRecentlyMentionedSkipsSelfSent(t *testing.T) {
	selfMention := timeline.RenderedSegment{ReceivedAtMs: 200, MentionsMe: true, IsSelfSent: true}
	ownMention := timeline.RenderedSegment{ReceivedAtMs: 300, RepliesToMe: true, IsMyself: true}
	rc := timeline.RenderedContext{selfMention, ownMention}

	if wasRecentlyMentioned(rc, 0) {
		t.Fatal("self-sent mentions must not wake the bot")
	}

	external := timeline.RenderedSegment{ReceivedAtMs: 400, MentionsMe: true}
	if !wasRecentlyMentioned(append(rc, external), 0) {
		t.Fatal("external mention must wake the bot")
	}
}

func TestBuildIgnoresMentionsCoveredByArtifacts(t *testing.T) {
	coveredMention := timeline.RenderedSegment{
		MessageID:    "m1",
		ReceivedAtMs: 100,
		MentionsMe:   true,
		Content:      []timeline.RenderedContentPiece{{Type: "text", Text: "old @bot ping"}},
	}
	rc := timeline.RenderedContext{
		coveredMention,
		{
			MessageID:    "m2",
			ReceivedAtMs: 200,
			Content:      []timeline.RenderedContentPiece{{Type: "text", Text: "unrelated chatter"}},
		},
	}
	artifacts := []timeline.CompactionArtifact{{
		ID:      "a1",
		Summary: "covers the old mention",
		Sources: []timeline.CompactionSource{{ExternalMessageID: "m1", CreatedAtMs: 100}},
	}}

	plan, ok := discussTriggerBuilder{}.Build(DiscussSessionConfig{ConversationType: "group"}, rc, nil, 0, artifacts)
	if !ok {
		t.Fatal("expected a composed plan")
	}
	if plan.command.DiscussMentioned {
		t.Fatal("compacted mention must not mark the session as mentioned")
	}

	uncovered := discussTriggerBuilder{}
	planLive, ok := uncovered.Build(DiscussSessionConfig{ConversationType: "group"}, rc, nil, 0, nil)
	if !ok {
		t.Fatal("expected a composed plan without artifacts")
	}
	if !planLive.command.DiscussMentioned {
		t.Fatal("uncovered mention must mark the session as mentioned")
	}
}
