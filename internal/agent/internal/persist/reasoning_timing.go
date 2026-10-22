package persist

import (
	"strings"
	"sync"
	"time"

	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

type reasoningTimingActiveSegment struct {
	startedAt  time.Time
	hasContent bool
}

// ReasoningTimingTracker observes the same normalized stream vocabulary used
// by Native and ACP. It keeps only the current, uncommitted model attempt so a
// retry cannot leak discarded timing into durable history.
type ReasoningTimingTracker struct {
	mu       sync.Mutex
	now      func() time.Time
	active   *reasoningTimingActiveSegment
	segments []messagepkg.ReasoningTimingSegment
}

func NewReasoningTimingTracker(now func() time.Time) *ReasoningTimingTracker {
	if now == nil {
		now = time.Now
	}
	return &ReasoningTimingTracker{now: now}
}

func (t *ReasoningTimingTracker) Observe(event native.StreamEvent) {
	if t == nil {
		return
	}
	// A concurrent tool's heartbeat does not end the model's thinking block.
	if event.Type == native.EventToolCallMetadata && event.Metadata["execution_progress"] != nil {
		return
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()

	switch event.Type {
	case native.EventRetry:
		// The terminal model transcript contains only the surviving attempt.
		// Drop both active and already-closed segments from the failed attempt.
		t.active = nil
		t.segments = nil
	case native.EventReasoningStart:
		t.finishActiveLocked(now, "completed")
		t.startLocked(now)
	case native.EventReasoningDelta:
		if t.active == nil {
			t.startLocked(now)
		}
		if strings.TrimSpace(event.Delta) != "" {
			t.active.hasContent = true
		}
	case native.EventReasoningEnd:
		t.finishActiveLocked(now, "completed")
	case native.EventAgentAbort:
		t.finishActiveLocked(now, "interrupted")
	case native.EventTextStart,
		native.EventTextDelta,
		native.EventTextEnd,
		native.EventToolCallInputStart,
		native.EventToolCallStart,
		native.EventToolCallMetadata,
		native.EventToolCallProgress,
		native.EventToolCallEnd,
		native.EventToolApprovalRequest,
		native.EventUserInputRequest,
		native.EventAttachment,
		native.EventReaction,
		native.EventSpeech,
		native.EventAgentEnd:
		t.finishActiveLocked(now, "completed")
	}
}

func (t *ReasoningTimingTracker) startLocked(now time.Time) {
	t.active = &reasoningTimingActiveSegment{startedAt: now}
}

func (t *ReasoningTimingTracker) finishActiveLocked(now time.Time, state string) {
	active := t.active
	t.active = nil
	if active == nil || !active.hasContent {
		return
	}
	duration := now.Sub(active.startedAt)
	if duration < 0 {
		duration = 0
	}
	t.segments = append(t.segments, messagepkg.ReasoningTimingSegment{
		DurationMS: duration.Milliseconds(),
		State:      state,
	})
}

// take closes an unterminated block at the persistence boundary, returns all
// segments for the current durable unit, and resets the unit for the next step.
func (t *ReasoningTimingTracker) take(state string) []messagepkg.ReasoningTimingSegment {
	if t == nil {
		return nil
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.finishActiveLocked(now, state)
	segments := t.segments
	t.segments = nil
	return segments
}

func TakeTerminalReasoningTiming(t *ReasoningTimingTracker, eventType native.StreamEventType) []messagepkg.ReasoningTimingSegment {
	state := "completed"
	if eventType == native.EventAgentAbort {
		state = "interrupted"
	}
	return t.take(state)
}

// ConfigureNativeReasoningTiming samples provider parts before Twilight's
// step buffer and binds the step committer, which consumes the timings inside
// its persistence barrier.
func ConfigureNativeReasoningTiming(
	cfg *native.RunConfig,
	tracker *ReasoningTimingTracker,
	stepCommitter *StepCommitter,
) {
	if cfg == nil || tracker == nil {
		return
	}
	previousObserver := cfg.OnProviderStreamEventObserved
	cfg.OnProviderStreamEventObserved = func(event native.StreamEvent) {
		if previousObserver != nil {
			previousObserver(event)
		}
		tracker.Observe(event)
	}
	stepCommitter.reasoningTiming = tracker
	cfg.OnStepCommitted = stepCommitter.commit
	cfg.OnStepInterrupted = stepCommitter.interrupt
}

func (opts RoundOptions) withReasoningTimingMetadata(messages []runreq.ModelMessage) RoundOptions {
	if len(opts.ReasoningTiming) == 0 || len(messages) == 0 {
		return opts
	}
	segmentIndex := 0
	for messageIndex, message := range messages {
		if !strings.EqualFold(strings.TrimSpace(message.Role), "assistant") {
			continue
		}
		rowSegments := make([]messagepkg.ReasoningTimingSegment, 0)
		for _, part := range message.ContentParts() {
			if !strings.EqualFold(strings.TrimSpace(part.Type), "reasoning") || strings.TrimSpace(part.Text) == "" {
				continue
			}
			if segmentIndex >= len(opts.ReasoningTiming) {
				break
			}
			segment := opts.ReasoningTiming[segmentIndex]
			segment.Ordinal = len(rowSegments)
			rowSegments = append(rowSegments, segment)
			segmentIndex++
		}
		if len(rowSegments) == 0 {
			continue
		}
		if opts.MessageMetadataByIndex == nil {
			opts.MessageMetadataByIndex = make(map[int]map[string]any, 1)
		}
		existing := opts.MessageMetadataByIndex[messageIndex]
		if existing == nil {
			existing = map[string]any{}
		}
		existing[messagepkg.ReasoningTimingMetadataKey] = messagepkg.ReasoningTimingMetadata{
			Version:  messagepkg.ReasoningTimingVersion,
			Segments: rowSegments,
		}
		opts.MessageMetadataByIndex[messageIndex] = existing
	}
	return opts
}
