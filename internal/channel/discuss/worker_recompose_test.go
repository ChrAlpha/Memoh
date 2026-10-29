package discuss

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/chat/timeline"
)

type countingArtifactProvider struct {
	calls atomic.Int64
}

type recoveringArtifactProvider struct {
	calls int
}

func (p *recoveringArtifactProvider) ActiveCompactionArtifacts(context.Context, string, string) ([]timeline.CompactionArtifact, error) {
	p.calls++
	summary := "small"
	if p.calls == 1 {
		summary = strings.Repeat("s", 8000)
	}
	return []timeline.CompactionArtifact{{ID: "summary", Summary: summary}}, nil
}

func TestDiscussChannelOverflowRequestsRecoveryWithoutPayload(t *testing.T) {
	artifacts := &recoveringArtifactProvider{}
	svc := &fakeTurnService{recomposeRuns: 1}
	svc.onStart = func(cmd turn.StartTurnCommand) {
		if svc.calls == 1 && (!cmd.DiscussContextOverflow || len(cmd.DiscussMessages) != 0 || cmd.DiscussContextTokens <= 1000 || cmd.DiscussCurrentTokens <= 0 || cmd.DiscussCurrentTokens >= 1000) {
			t.Errorf("overflow must carry recovery metadata only: overflow=%v messages=%d pressure=%d", cmd.DiscussContextOverflow, len(cmd.DiscussMessages), cmd.DiscussContextTokens)
		}
	}
	driver := NewDiscussDriver(DiscussDriverDeps{Artifacts: artifacts, AdmissionMaxTokens: 1000})
	sess := &discussSession{config: DiscussSessionConfig{BotID: "bot-1", ThreadID: "sess-1"}}
	driver.handleReplyWithTurn(t.Context(), sess, recomposeTestRC(), driver.logger, svc)
	if svc.calls != 2 || sess.lastProcessed.SourceCursor != 200 || svc.lastCmd.DiscussContextOverflow {
		t.Fatalf("recovery failed: calls=%d cursor=%+v overflow=%v", svc.calls, sess.lastProcessed, svc.lastCmd.DiscussContextOverflow)
	}
}

func TestDiscussChannelOverflowRecoveryIsBoundedAndDoesNotConsume(t *testing.T) {
	for _, retries := range []int{0, 99} {
		svc := &fakeTurnService{recomposeRuns: retries}
		artifacts := &fakeArtifactProvider{artifacts: []timeline.CompactionArtifact{{ID: "summary", Summary: strings.Repeat("s", 8000)}}}
		driver := NewDiscussDriver(DiscussDriverDeps{Artifacts: artifacts, AdmissionMaxTokens: 1000})
		sess := &discussSession{config: DiscussSessionConfig{BotID: "bot-1", ThreadID: "sess-1"}}
		driver.handleReplyWithTurn(t.Context(), sess, recomposeTestRC(), driver.logger, svc)
		want := 1
		if retries > 0 {
			want = (maxDiscussRecoveries + 1)
		}
		if svc.calls != want || sess.lastProcessed.SourceCursor != 0 || len(svc.lastCmd.DiscussMessages) != 0 {
			t.Fatalf("unsafe overflow retry: calls=%d want=%d cursor=%+v payloads=%d", svc.calls, want, sess.lastProcessed, len(svc.lastCmd.DiscussMessages))
		}
	}
}

func (p *countingArtifactProvider) ActiveCompactionArtifacts(context.Context, string, string) ([]timeline.CompactionArtifact, error) {
	p.calls.Add(1)
	return nil, nil
}

func recomposeTestRC() timeline.RenderedContext {
	return timeline.RenderedContext{
		{
			ReceivedAtMs: 200,
			Content:      []timeline.RenderedContentPiece{{Type: "text", Text: `<message id="1">hello</message>`}},
		},
	}
}

func TestHandleReplyWithTurn_RecomposeReloadsArtifactsAndResubmits(t *testing.T) {
	svc := &fakeTurnService{recomposeRuns: 1}
	artifacts := &countingArtifactProvider{}
	driver := NewDiscussDriver(DiscussDriverDeps{Artifacts: artifacts})
	sess := &discussSession{
		config: DiscussSessionConfig{BotID: "bot-1", ThreadID: "sess-1"},
	}

	driver.handleReplyWithTurn(context.Background(), sess, recomposeTestRC(), driver.logger, svc)

	if svc.calls != 2 {
		t.Fatalf("StartTurn calls = %d, want 2 (recompose then rerun)", svc.calls)
	}
	if got := artifacts.calls.Load(); got != 2 {
		t.Fatalf("artifact loads = %d, want 2 (frontier reloaded per attempt)", got)
	}
	if sess.lastProcessed.SourceCursor != 200 {
		t.Fatalf("cursor must advance after the rerun completes, got %+v", sess.lastProcessed)
	}
}

type countingHistoryReader struct {
	lightweightHistoryStub
	calls int
}

func (h *countingHistoryReader) ListTurnResponseSourcesSinceBySessionWithinBytes(ctx context.Context, sessionID string, since time.Time, maxBytes int64) ([]messagepkg.Message, error) {
	h.calls++
	return h.lightweightHistoryStub.ListTurnResponseSourcesSinceBySessionWithinBytes(ctx, sessionID, since, maxBytes)
}

// The turn responses a trigger composes are decoded against the frontier it
// composes with, so a recompose after a compaction reads both again.
func TestHandleReplyWithTurn_RecomposeReloadsTurnResponsesWithTheFrontier(t *testing.T) {
	svc := &fakeTurnService{recomposeRuns: 1}
	artifacts := &countingArtifactProvider{}
	history := &countingHistoryReader{}
	driver := NewDiscussDriver(DiscussDriverDeps{Artifacts: artifacts, MessageService: history})
	sess := &discussSession{config: DiscussSessionConfig{BotID: "bot-1", ThreadID: "sess-1"}}

	driver.handleReplyWithTurn(context.Background(), sess, recomposeTestRC(), driver.logger, svc)

	if svc.calls != 2 || artifacts.calls.Load() != 2 || history.calls != 2 {
		t.Fatalf("runs=%d frontier loads=%d history loads=%d, want 2 each", svc.calls, artifacts.calls.Load(), history.calls)
	}
}

// The cold-start anchor reads every persisted answer, including one a summary
// replaces: leaving it out would replay context the bot already answered.
func TestHandleReplyWithTurn_ColdStartAnchorReadsCoveredAnswers(t *testing.T) {
	answered := timeline.RenderedSegment{
		MessageID:    "m1",
		ReceivedAtMs: 250,
		Content:      []timeline.RenderedContentPiece{{Type: "text", Text: `<message id="m1">question</message>`}},
	}
	frontier := &fakeArtifactProvider{artifacts: []timeline.CompactionArtifact{{
		ID: "a1", Summary: "folded", AnchorEndMs: 400,
		Sources: []timeline.CompactionSource{
			{HistoryMessageID: "answer", CreatedAtMs: 300},
			{ExternalMessageID: "m0", CreatedAtMs: 400},
		},
	}}}
	history := &lightweightHistoryStub{messages: []messagepkg.Message{{
		ID: "answer", Role: "assistant", CreatedAt: time.UnixMilli(300),
		Content: []byte(`{"role":"assistant","content":"answer"}`),
	}}}
	svc := &fakeTurnService{}
	driver := NewDiscussDriver(DiscussDriverDeps{Artifacts: frontier, MessageService: history, CursorStore: &fakeDiscussCursorStore{}})
	sess := &discussSession{config: DiscussSessionConfig{BotID: "b", ThreadID: "s"}}

	driver.handleReplyWithTurn(context.Background(), sess, timeline.RenderedContext{answered}, driver.logger, svc)

	if svc.calls != 0 || sess.lastProcessed.SourceCursor != 300 {
		t.Fatalf("calls=%d cursor=%+v, want the covered answer to anchor the cursor", svc.calls, sess.lastProcessed)
	}
}

func TestHandleReplyWithTurn_RecomposeLimitDefersToNextTrigger(t *testing.T) {
	svc := &fakeTurnService{recomposeRuns: 99}
	driver := NewDiscussDriver(DiscussDriverDeps{})
	sess := &discussSession{
		config: DiscussSessionConfig{BotID: "bot-1", ThreadID: "sess-1"},
	}

	driver.handleReplyWithTurn(context.Background(), sess, recomposeTestRC(), driver.logger, svc)

	if svc.calls != (maxDiscussRecoveries + 1) {
		t.Fatalf("StartTurn calls = %d, want %d", svc.calls, (maxDiscussRecoveries + 1))
	}
	if sess.lastProcessed.SourceCursor == 200 {
		t.Fatal("cursor must not advance when every attempt ended in recompose")
	}
}

func TestHandleReplyWithTurnRecoveryCompletesOriginalInput(t *testing.T) {
	svc := &fakeTurnService{recomposeRuns: 1}
	svc.onStart = func(cmd turn.StartTurnCommand) {
		if cmd.DiscussRecoveryExhausted != (svc.calls == 2) {
			t.Errorf("attempt %d exhausted=%v", svc.calls, cmd.DiscussRecoveryExhausted)
		}
	}
	driver := NewDiscussDriver(DiscussDriverDeps{})
	sess := &discussSession{config: DiscussSessionConfig{BotID: "bot-1", ThreadID: "sess-1"}}
	driver.handleReplyWithTurn(t.Context(), sess, recomposeTestRC(), driver.logger, svc)
	if svc.calls != 2 || sess.lastProcessed.SourceCursor != 200 {
		t.Fatalf("calls=%d cursor=%+v; the attempt after recovery must complete original input", svc.calls, sess.lastProcessed)
	}
	driver.handleReplyWithTurn(t.Context(), sess, recomposeTestRC(), driver.logger, svc)
	if svc.calls != 2 {
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

type failingArtifactProvider struct{}

func (failingArtifactProvider) ActiveCompactionArtifacts(context.Context, string, string) ([]timeline.CompactionArtifact, error) {
	return nil, errors.New("frontier unavailable")
}

type failingHistoryReader struct{}

func (failingHistoryReader) ListTurnResponseSourcesSinceBySessionWithinBytes(context.Context, string, time.Time, int64) ([]messagepkg.Message, error) {
	return nil, errors.New("history unavailable")
}

func TestDiscussMissingSourceFailsWithoutRunningOrConsuming(t *testing.T) {
	for name, deps := range map[string]DiscussDriverDeps{
		"artifacts": {Artifacts: failingArtifactProvider{}},
		"history":   {MessageService: &failingHistoryReader{}},
	} {
		t.Run(name, func(t *testing.T) {
			broadcaster := &recordingBroadcaster{}
			deps.Broadcaster = broadcaster
			svc := &fakeTurnService{}
			driver := NewDiscussDriver(deps)
			sess := &discussSession{config: DiscussSessionConfig{BotID: "bot-1", ThreadID: "sess-1"}}
			driver.handleReplyWithTurn(t.Context(), sess, recomposeTestRC(), driver.logger, svc)
			failures := broadcaster.errors()
			if svc.calls != 0 || sess.lastProcessed.SourceCursor != 0 || len(failures) != 1 || failures[0].ErrorCode != string(apperror.CodeInternal) {
				t.Fatalf("calls=%d cursor=%+v failures=%+v, want one internal failure and nothing run or consumed", svc.calls, sess.lastProcessed, failures)
			}
		})
	}
}
