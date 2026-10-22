package persist

import (
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/step"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

type reasoningTimingTestClock struct {
	now time.Time
}

func newReasoningTimingTestClock() *reasoningTimingTestClock {
	return &reasoningTimingTestClock{now: time.Date(2026, 8, 26, 1, 2, 3, 0, time.UTC)}
}

func (c *reasoningTimingTestClock) read() time.Time {
	return c.now
}

func (c *reasoningTimingTestClock) advance(duration time.Duration) {
	c.now = c.now.Add(duration)
}

func TestReasoningTimingTrackerMeasuresExplicitBlock(t *testing.T) {
	t.Parallel()

	clock := newReasoningTimingTestClock()
	tracker := NewReasoningTimingTracker(clock.read)
	tracker.Observe(native.StreamEvent{Type: native.EventReasoningStart})
	clock.advance(400 * time.Millisecond)
	tracker.Observe(native.StreamEvent{Type: native.EventReasoningDelta, Delta: "inspect"})
	tracker.Observe(native.StreamEvent{Type: native.EventToolCallMetadata, Metadata: map[string]any{
		"execution_progress": map[string]any{"elapsed_time_seconds": float64(1)},
	}})
	clock.advance(1600 * time.Millisecond)
	tracker.Observe(native.StreamEvent{Type: native.EventReasoningEnd})

	segments := tracker.take("completed")
	if len(segments) != 1 {
		t.Fatalf("segments = %#v, want one", segments)
	}
	got := segments[0]
	if got.DurationMS != 2000 || got.State != "completed" {
		t.Fatalf("segment duration/state = %#v", got)
	}
}

func TestReasoningTimingTrackerDeltaOnlyFallback(t *testing.T) {
	t.Parallel()

	clock := newReasoningTimingTestClock()
	tracker := NewReasoningTimingTracker(clock.read)
	tracker.Observe(native.StreamEvent{Type: native.EventReasoningDelta, Delta: "plan"})
	clock.advance(1250 * time.Millisecond)
	tracker.Observe(native.StreamEvent{Type: native.EventReasoningDelta, Delta: " more"})
	clock.advance(750 * time.Millisecond)
	tracker.Observe(native.StreamEvent{Type: native.EventTextDelta, Delta: "answer"})

	segments := tracker.take("completed")
	if len(segments) != 1 {
		t.Fatalf("segments = %#v, want one", segments)
	}
	if got := segments[0]; got.DurationMS != 2000 || got.State != "completed" {
		t.Fatalf("delta-only segment = %#v", got)
	}
}

func TestReasoningTimingTrackerRetryKeepsOnlySurvivingAttempt(t *testing.T) {
	t.Parallel()

	clock := newReasoningTimingTestClock()
	tracker := NewReasoningTimingTracker(clock.read)
	tracker.Observe(native.StreamEvent{Type: native.EventReasoningDelta, Delta: "discarded"})
	clock.advance(time.Second)
	tracker.Observe(native.StreamEvent{Type: native.EventReasoningEnd})
	tracker.Observe(native.StreamEvent{Type: native.EventRetry})
	clock.advance(time.Second)
	tracker.Observe(native.StreamEvent{Type: native.EventReasoningDelta, Delta: "survives"})
	clock.advance(3 * time.Second)
	tracker.Observe(native.StreamEvent{Type: native.EventReasoningEnd})

	segments := tracker.take("completed")
	if len(segments) != 1 {
		t.Fatalf("segments = %#v, want surviving attempt only", segments)
	}
	if got := segments[0]; got.DurationMS != 3000 {
		t.Fatalf("surviving segment = %#v", got)
	}
}

// Each committed step takes the timing of its own model call, so a later
// provider call cannot erase an earlier tool-loop step as if it were a retry.
func TestConfigureNativeReasoningTimingKeepsCompletedStepsAcrossProviderCalls(t *testing.T) {
	fx := newSteerFixture(t)
	handle := fx.handle
	ctx := replacementTestContext(fx)
	committer := fx.writer.NewStepCommitter(ctx, runreq.ChatRequest{
		BotID: handle.BotID, ThreadID: handle.SessionID, RunID: handle.RunID, RunHandle: handle,
		TurnID: "turn-1", TurnPosition: int64Pointer(1), Query: "hello",
	}, replacementResolvedContext())
	if committer == nil {
		t.Fatal("missing step committer")
	}
	clock := newReasoningTimingTestClock()
	tracker := NewReasoningTimingTracker(clock.read)
	cfg := native.RunConfig{}
	ConfigureNativeReasoningTiming(&cfg, tracker, committer)

	reasoningStep := func(text string) *step.Record {
		return &step.Record{Messages: []sdk.Message{{
			Role:    sdk.MessageRoleAssistant,
			Content: []sdk.MessagePart{sdk.ReasoningPart{Text: text}, sdk.TextPart{Text: "answer"}},
		}}}
	}
	cfg.OnProviderStreamEventObserved(native.StreamEvent{Type: native.EventRetry})
	cfg.OnProviderStreamEventObserved(native.StreamEvent{Type: native.EventReasoningDelta, Delta: "first step"})
	clock.advance(2 * time.Second)
	if _, err := cfg.OnStepCommitted(ctx, 0, reasoningStep("first step")); err != nil {
		t.Fatalf("commit first step: %v", err)
	}

	cfg.OnProviderStreamEventObserved(native.StreamEvent{Type: native.EventRetry})
	cfg.OnProviderStreamEventObserved(native.StreamEvent{Type: native.EventReasoningDelta, Delta: "second step"})
	clock.advance(3 * time.Second)
	if _, err := cfg.OnStepCommitted(ctx, 1, reasoningStep("second step")); err != nil {
		t.Fatalf("commit second step: %v", err)
	}

	steps := fx.messages.Steps
	if len(steps) != 2 {
		t.Fatalf("committed steps = %d, want 2", len(steps))
	}
	for i, want := range []int64{2000, 3000} {
		rows := steps[i].Messages
		segments := messagepkg.ReasoningTimingFromMetadata(rows[len(rows)-1].Metadata)
		if len(segments) != 1 || segments[0].DurationMS != want {
			t.Fatalf("step %d reasoning timing = %#v, want one segment of %dms", i, segments, want)
		}
	}
}

func TestReasoningTimingTrackerMarksOpenSegmentInterrupted(t *testing.T) {
	t.Parallel()

	clock := newReasoningTimingTestClock()
	tracker := NewReasoningTimingTracker(clock.read)
	tracker.Observe(native.StreamEvent{Type: native.EventReasoningDelta, Delta: "partial"})
	clock.advance(2300 * time.Millisecond)

	segments := tracker.take("interrupted")
	if len(segments) != 1 {
		t.Fatalf("segments = %#v, want one", segments)
	}
	if got := segments[0]; got.DurationMS != 2300 || got.State != "interrupted" {
		t.Fatalf("interrupted segment = %#v", got)
	}
}

func TestReasoningTimingTrackerOmitsUnmeasuredEmptyReasoning(t *testing.T) {
	t.Parallel()

	clock := newReasoningTimingTestClock()
	tracker := NewReasoningTimingTracker(clock.read)
	tracker.Observe(native.StreamEvent{Type: native.EventReasoningStart})
	clock.advance(time.Second)
	tracker.Observe(native.StreamEvent{Type: native.EventReasoningDelta, Delta: "  "})
	tracker.Observe(native.StreamEvent{Type: native.EventReasoningEnd})
	if got := tracker.take("completed"); len(got) != 0 {
		t.Fatalf("empty reasoning timing = %#v, want absent", got)
	}
}

func TestStoreRoundAttachesReasoningTimingAfterPersistedUserIsSkipped(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	service := newTestWriter(Deps{Messages: messages})
	assistant := sdk.Message{
		Role: sdk.MessageRoleAssistant,
		Content: []sdk.MessagePart{
			sdk.ReasoningPart{Text: "thinking"},
			sdk.TextPart{Text: "answer"},
		},
	}
	round := append([]runreq.ModelMessage{{Role: "user", Content: runreq.NewTextContent("hello")}}, runreq.SDKMessagesToModelMessages([]sdk.Message{assistant})...)
	segment := messagepkg.ReasoningTimingSegment{
		DurationMS: 2000,
		State:      "completed",
	}

	_, err := service.StoreRoundResult(t.Context(), runreq.ChatRequest{
		BotID:                "bot-1",
		ThreadID:             "session-1",
		Query:                "hello",
		UserMessagePersisted: true,
	}, round, "model-1", RoundOptions{ReasoningTiming: []messagepkg.ReasoningTimingSegment{segment}})
	if err != nil {
		t.Fatalf("storeRoundWithOptionsResult() error = %v", err)
	}
	if len(messages.Persisted) != 1 || messages.Persisted[0].Role != "assistant" {
		t.Fatalf("persisted = %#v, want one assistant row", messages.Persisted)
	}
	segments := messagepkg.ReasoningTimingFromMetadata(messages.Persisted[0].Metadata)
	if len(segments) != 1 || segments[0].DurationMS != 2000 || segments[0].Ordinal != 0 {
		t.Fatalf("persisted reasoning timing = %#v", segments)
	}
}
