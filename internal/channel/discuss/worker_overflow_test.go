package discuss

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/felinics/memoh/internal/agent/turn"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/chat/timeline"
)

type countingArtifactProvider struct {
	calls     atomic.Int64
	artifacts []timeline.CompactionArtifact
}

func (p *countingArtifactProvider) ActiveCompactionArtifacts(context.Context, string, string) ([]timeline.CompactionArtifact, error) {
	p.calls.Add(1)
	return p.artifacts, nil
}

func overflowTestRC() timeline.RenderedContext {
	return timeline.RenderedContext{
		{
			ReceivedAtMs: 200,
			Content:      []timeline.RenderedContentPiece{{Type: "text", Text: `<message id="1">hello</message>`}},
		},
	}
}

func TestDiscussChannelOverflowStartsOneMetadataOnlyTurnWithoutConsuming(t *testing.T) {
	svc := &fakeTurnService{}
	svc.onStart = func(cmd turn.StartTurnCommand) {
		if !cmd.DiscussContextOverflow || len(cmd.DiscussMessages) != 0 || cmd.DiscussContextTokens <= 1000 || cmd.DiscussCurrentTokens <= 0 || cmd.DiscussCurrentTokens >= 1000 {
			t.Errorf("overflow must carry pressure metadata only: overflow=%v messages=%d pressure=%d current=%d", cmd.DiscussContextOverflow, len(cmd.DiscussMessages), cmd.DiscussContextTokens, cmd.DiscussCurrentTokens)
		}
	}
	artifacts := &countingArtifactProvider{artifacts: []timeline.CompactionArtifact{{ID: "summary", Summary: strings.Repeat("s", 8000)}}}
	driver := NewDiscussDriver(DiscussDriverDeps{Artifacts: artifacts, AdmissionMaxTokens: 1000})
	sess := &discussSession{config: DiscussSessionConfig{BotID: "bot-1", ThreadID: "sess-1"}}
	driver.handleReplyWithTurn(t.Context(), sess, overflowTestRC(), driver.logger, svc)
	if svc.calls != 1 || artifacts.calls.Load() != 1 || sess.lastProcessed.SourceCursor != 0 {
		t.Fatalf("calls=%d artifact loads=%d cursor=%+v; overflow must run once and leave the input unconsumed", svc.calls, artifacts.calls.Load(), sess.lastProcessed)
	}
}

func TestDiscussTriggerRunsOnceAndConsumesCompletedInput(t *testing.T) {
	svc := &fakeTurnService{}
	artifacts := &countingArtifactProvider{}
	driver := NewDiscussDriver(DiscussDriverDeps{Artifacts: artifacts})
	sess := &discussSession{config: DiscussSessionConfig{BotID: "bot-1", ThreadID: "sess-1"}}
	driver.handleReplyWithTurn(t.Context(), sess, overflowTestRC(), driver.logger, svc)
	if svc.calls != 1 || artifacts.calls.Load() != 1 || sess.lastProcessed.SourceCursor != 200 {
		t.Fatalf("calls=%d artifact loads=%d cursor=%+v", svc.calls, artifacts.calls.Load(), sess.lastProcessed)
	}
	driver.handleReplyWithTurn(t.Context(), sess, overflowTestRC(), driver.logger, svc)
	if svc.calls != 1 {
		t.Fatal("same input processed twice")
	}
}

func TestDiscussSkippedAgentTurnConsumesOverflowingInput(t *testing.T) {
	svc := &fakeTurnService{runtimeType: sessionpkg.RuntimeACPAgent}
	driver := NewDiscussDriver(DiscussDriverDeps{AdmissionMaxTokens: 100})
	sess := &discussSession{config: DiscussSessionConfig{BotID: "bot-1", ThreadID: "sess-1", ConversationType: "group"}}
	rc := timeline.RenderedContext{{MessageID: "big", ReceivedAtMs: 200, Content: []timeline.RenderedContentPiece{{Type: "text", Text: strings.Repeat("x", 2000)}}}}
	driver.handleReplyWithTurn(t.Context(), sess, rc, driver.logger, svc)
	if svc.calls != 1 || !svc.lastCmd.DiscussContextOverflow || sess.lastProcessed.SourceCursor != 200 {
		t.Fatalf("calls=%d overflow=%v cursor=%+v; an unaddressed agent turn must consume what it skips", svc.calls, svc.lastCmd.DiscussContextOverflow, sess.lastProcessed)
	}
}
