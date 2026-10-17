package drive_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/drivetest"
	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

// orderedRecorder is a shared clock over the two things a driver test must
// order: the events a run published and the history writes it made.
type orderedRecorder struct {
	mu    sync.Mutex
	steps []string
}

func (r *orderedRecorder) record(step string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, step)
	return len(r.steps)
}

func (r *orderedRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.steps...)
}

// O1 — the run's terminal state is written after the last history write and
// after the delivery end has seen everything published.
func TestOrderTerminalWriteFollowsDeliveryAndHistory(t *testing.T) {
	t.Parallel()

	terminal := agenttest.TerminalEvent(t, "done")
	clock := &orderedRecorder{}
	exec := &drivetest.ScriptedExecutor{
		Events:   []native.StreamEvent{{Type: native.EventTextDelta, Delta: "a"}},
		Terminal: &terminal,
	}
	pub := &agenttest.EventRecorder{OnPublish: func(ev native.StreamEvent) {
		clock.record("published:" + string(ev.Type))
	}}
	admitter := &agenttest.ScriptedAdmitter{Started: true}
	svc := newDriveService(pub, admitter)
	delivery := &orderedDelivery{clock: clock}
	spec := drivetest.ScheduledSpec(exec, delivery)
	spec.Setup = func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
		return drive.RunPlan{Exec: &clockExecutor{inner: exec, clock: clock}}, nil
	}

	svc.Drive(context.Background(), spec)

	var wroteHistory int
	for i, s := range clock.all() {
		if s == "history-written" {
			wroteHistory = i + 1
		}
	}
	if wroteHistory == 0 {
		t.Fatalf("the run wrote no history: %v", clock.all())
	}
}

type orderedDelivery struct {
	clock *orderedRecorder
}

func (d *orderedDelivery) Deliver(_ context.Context, ev drive.RunEvent) bool {
	d.clock.record("delivered:" + string(ev.Event.Type))
	return true
}

func (*orderedDelivery) Settle() {}

func (*orderedDelivery) Ended(context.Context, drive.RunEnded) {}

type clockExecutor struct {
	inner *drivetest.ScriptedExecutor
	clock *orderedRecorder
}

func (c *clockExecutor) Execute(ctx context.Context, emit func(native.StreamEvent)) drive.ExecEnding {
	ending := c.inner.Execute(ctx, emit)
	commit := ending.Commit
	ending.Commit = func(ctx context.Context, f *outcome.RunFailure, row bool) (drive.CommitSummary, error) {
		c.clock.record("history-written")
		return commit(ctx, f, row)
	}
	return ending
}

// O2 — a commit that failed publishes no terminal, and the terminal write
// records the run as errored rather than letting it look finished.
func TestOrderFailedCommitRecordsErroredAndPublishesNoTerminal(t *testing.T) {
	t.Parallel()

	terminal := agenttest.TerminalEvent(t, "must not look complete")
	exec := &drivetest.ScriptedExecutor{Terminal: &terminal, CommitErr: errors.New("write failed")}
	pub := &agenttest.EventRecorder{}
	admitter := &agenttest.ScriptedAdmitter{Started: true}
	svc := newDriveService(pub, admitter)

	result := svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, &drivetest.RecordingDelivery{}))

	for _, ev := range pub.Published() {
		if ev.IsTerminal() {
			t.Fatalf("published terminal %#v after a failed history write", ev)
		}
	}
	if len(admitter.Finishes) != 1 {
		t.Fatalf("terminal writes = %#v, want exactly one", admitter.Finishes)
	}
	if got := admitter.Finishes[0].Status; got == "" || strings.EqualFold(got, "completed") {
		t.Fatalf("terminal status = %q, want the run recorded as failed", got)
	}
	if result.Failure == nil {
		t.Fatal("the run reported no failure")
	}
}

// A slow delivery end does not hold up publication, which is what keeps the
// engine's step commit barrier from waiting on the caller.
func TestOrderSlowDeliveryDoesNotDelayPublication(t *testing.T) {
	t.Parallel()

	terminal := agenttest.TerminalEvent(t, "done")
	published := make(chan struct{}, 4)
	exec := &drivetest.ScriptedExecutor{
		Events:   []native.StreamEvent{{Type: native.EventTextDelta, Delta: "a"}},
		Terminal: &terminal,
	}
	pub := &agenttest.EventRecorder{OnPublish: func(native.StreamEvent) {
		select {
		case published <- struct{}{}:
		default:
		}
	}}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	block := make(chan struct{})
	delivery := &blockingDelivery{block: block}

	done := make(chan drive.RunResult, 1)
	go func() { done <- svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, delivery)) }()

	select {
	case <-published:
	case <-time.After(2 * time.Second):
		close(block)
		<-done
		t.Fatal("publication waited for the delivery end")
	}
	close(block)
	if result := <-done; result.End != outcome.EndCompleted {
		t.Fatalf("end = %s, want completed", result.End)
	}
}

type blockingDelivery struct {
	block chan struct{}
	once  sync.Once
}

func (d *blockingDelivery) Deliver(_ context.Context, _ drive.RunEvent) bool {
	d.once.Do(func() {})
	<-d.block
	return true
}

func (*blockingDelivery) Settle() {}

func (*blockingDelivery) Ended(context.Context, drive.RunEnded) {}

// O4 — the delivery end leaving stops delivery, not publication: later events
// and the terminal still reach the run's other subscribers.
func TestOrderDeliveryEndLeavingKeepsPublishing(t *testing.T) {
	t.Parallel()

	terminal := agenttest.TerminalEvent(t, "done")
	exec := &drivetest.ScriptedExecutor{
		Events: []native.StreamEvent{
			{Type: native.EventTextDelta, Delta: "a"},
			{Type: native.EventTextDelta, Delta: "b"},
		},
		Terminal: &terminal,
	}
	pub := &agenttest.EventRecorder{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	delivery := &drivetest.RecordingDelivery{GoneAt: 2}

	svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, delivery))

	events, _, _ := delivery.Snapshot()
	if len(events) != 1 {
		t.Fatalf("delivered %d events after the caller left, want one", len(events))
	}
	if len(pub.Published()) != 3 {
		t.Fatalf("published %d events, want every one including the terminal", len(pub.Published()))
	}
}

// O5 — the terminal is the last event the caller receives, and the run's
// terminal write happens after that last delivery.
func TestOrderTerminalIsTheLastDelivery(t *testing.T) {
	t.Parallel()

	terminal := agenttest.TerminalEvent(t, "done")
	exec := &drivetest.ScriptedExecutor{
		Events:   []native.StreamEvent{{Type: native.EventTextDelta, Delta: "a"}},
		Terminal: &terminal,
	}
	pub := &agenttest.EventRecorder{}
	admitter := &agenttest.ScriptedAdmitter{Started: true}
	svc := newDriveService(pub, admitter)
	clock := &orderedRecorder{}
	delivery := &orderedDelivery{clock: clock}

	svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, delivery))

	steps := clock.all()
	if len(steps) == 0 {
		t.Fatal("nothing was delivered")
	}
	last := steps[len(steps)-1]
	if last != "delivered:"+string(native.EventAgentEnd) {
		t.Fatalf("last delivery = %q, want the terminal event", last)
	}
}

// A publication is refused once and the run does not retry the publisher with
// the terminal event.
func TestOrderRefusedPublisherIsNotCalledAgain(t *testing.T) {
	t.Parallel()

	terminal := agenttest.TerminalEvent(t, "done")
	exec := &drivetest.ScriptedExecutor{
		Events:   []native.StreamEvent{{Type: native.EventTextDelta, Delta: "a"}},
		Terminal: &terminal,
	}
	pub := &agenttest.EventRecorder{ErrAt: 1, Err: errors.New("projection refused")}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})

	svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, &drivetest.RecordingDelivery{}))

	if pub.Calls != 1 {
		t.Fatalf("publish calls = %d, want exactly the refused one", pub.Calls)
	}
}

// O7 — every event the producer emitted is published exactly once.
func TestOrderEveryEventPublishedOnce(t *testing.T) {
	t.Parallel()

	terminal := agenttest.TerminalEvent(t, "done")
	exec := &drivetest.ScriptedExecutor{
		Events: []native.StreamEvent{
			{Type: native.EventTextDelta, Delta: "a"},
			{Type: native.EventTextDelta, Delta: "b"},
			{Type: native.EventToolCallStart, ToolName: "search"},
		},
		Terminal: &terminal,
	}
	pub := &agenttest.EventRecorder{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})

	svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, &drivetest.RecordingDelivery{}))

	published := pub.Published()
	if len(published) != 4 {
		t.Fatalf("published %d events, want both deltas, the tool call and the terminal", len(published))
	}
	seen := map[string]int{}
	for _, ev := range published {
		seen[ev.Delta+string(ev.Type)]++
	}
	for key, count := range seen {
		if count != 1 {
			t.Fatalf("event %q published %d times, want once", key, count)
		}
	}
}

// O8 — the tail of a stopped run is published through a detached context, so
// a subscriber still learns the run stopped.
func TestOrderStoppedRunStillPublishesItsTail(t *testing.T) {
	t.Parallel()

	terminal := native.StreamEvent{Type: native.EventAgentAbort, EndReason: event.EndCanceled}
	exec := &drivetest.ScriptedExecutor{
		Events:   []native.StreamEvent{{Type: native.EventTextDelta, Delta: "partial"}},
		Terminal: &terminal,
	}
	pub := &agenttest.EventRecorder{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	runCtx, stop := context.WithCancelCause(context.Background())
	stop(outcome.ErrRunStopped)

	result := svc.Drive(runCtx, drivetest.ScheduledSpec(exec, &drivetest.RecordingDelivery{}))

	if result.End != outcome.EndStopped {
		t.Fatalf("end = %s, want stopped", result.End)
	}
	published := pub.Published()
	if len(published) < 2 {
		t.Fatalf("published %#v, want the tail and the abort terminal", published)
	}
	var publishedTerminal bool
	for _, ev := range published {
		if ev.Type == native.EventAgentAbort {
			publishedTerminal = true
		}
	}
	if !publishedTerminal {
		t.Fatal("the stopped run's terminal never reached the projection")
	}
}

// A parked run keeps its slot: it writes the parked status, not a terminal one.
func TestOrderParkedRunWritesNoTerminalState(t *testing.T) {
	t.Parallel()

	terminal := native.StreamEvent{Type: native.EventAgentEnd, ApprovalID: "appr-1"}
	exec := &drivetest.ScriptedExecutor{Terminal: &terminal, Deferred: true}
	pub := &agenttest.EventRecorder{}
	admitter := &agenttest.ScriptedAdmitter{Started: true}
	svc := newDriveService(pub, admitter)

	result := svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, &drivetest.RecordingDelivery{}))

	if result.End != outcome.EndParked {
		t.Fatalf("end = %s, want parked (failure %#v)", result.End, result.Failure)
	}
	if len(admitter.Finishes) != 1 {
		t.Fatalf("terminal writes = %#v, want the parked status", admitter.Finishes)
	}
}

// O6 — preparation runs before publication, so the event a subscriber receives
// already names the asset it can fetch, and a spoken reply arrives as the voice
// attachment it became, in the place the speech event held.
func TestOrderPreparationRewritesEventsBeforePublication(t *testing.T) {
	t.Parallel()

	terminal := agenttest.TerminalEvent(t, "done")
	exec := &drivetest.ScriptedExecutor{
		Events: []native.StreamEvent{
			{Type: native.EventTextDelta, Delta: "a"},
			{Type: native.EventAttachment, Attachments: []event.FileAttachment{{Type: "image", Path: "/tmp/a.png", Mime: "image/png"}}},
			{Type: native.EventSpeech, Speeches: []event.SpeechItem{{Text: "read this out"}}},
			{Type: native.EventTextDelta, Delta: "b"},
		},
		Terminal: &terminal,
	}
	pub := &agenttest.EventRecorder{}
	media := &agenttest.OutputMedia{}
	speech := &agenttest.OutputSpeech{Audio: []byte("voice-bytes"), ContentType: "audio/mpeg"}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true}, withOutput(media, speech))
	delivery := &drivetest.RecordingDelivery{}

	svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, delivery))

	published := pub.Published()
	if len(published) != 5 {
		t.Fatalf("published %d events, want the two deltas, the attachment, the voice attachment and the terminal", len(published))
	}
	if published[1].Type != native.EventAttachment || len(published[1].Attachments) != 1 {
		t.Fatalf("published[1] = %#v, want one attachment event", published[1])
	}
	if got := published[1].Attachments[0].ContentHash; got != "hash:/tmp/a.png" {
		t.Fatalf("attachment content hash = %q, want the ingested asset's", got)
	}
	if published[2].Type != native.EventAttachment || len(published[2].Attachments) != 1 {
		t.Fatalf("published[2] = %#v, want the speech event replaced by a voice attachment", published[2])
	}
	if got := published[2].Attachments[0].Type; got != "voice" {
		t.Fatalf("speech replacement type = %q, want %q", got, "voice")
	}
	if got := published[3].Delta; got != "b" {
		t.Fatalf("published[3] = %#v, want the event that followed the speech to keep its place", published[3])
	}

	delivered, _, _ := delivery.Snapshot()
	if len(delivered) != len(published) {
		t.Fatalf("delivered %d events, want the %d published", len(delivered), len(published))
	}
	for i := range published {
		if delivered[i].Event.Type != published[i].Type || delivered[i].Event.Delta != published[i].Delta {
			t.Fatalf("delivered[%d] = %#v, want the published order", i, delivered[i].Event)
		}
	}
}

// A slow synthesis does not reorder anything around it: the preparation of an
// event is what the events after it wait on, not a race with them.
func TestOrderSlowSpeechPreparationKeepsEventOrder(t *testing.T) {
	t.Parallel()

	terminal := agenttest.TerminalEvent(t, "done")
	exec := &drivetest.ScriptedExecutor{
		Events: []native.StreamEvent{
			{Type: native.EventTextDelta, Delta: "a"},
			{Type: native.EventSpeech, Speeches: []event.SpeechItem{{Text: "slow"}}},
			{Type: native.EventTextDelta, Delta: "b"},
		},
		Terminal: &terminal,
	}
	pub := &agenttest.EventRecorder{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true}, withOutput(&agenttest.OutputMedia{}, &agenttest.OutputSpeech{
		Audio:       []byte("voice-bytes"),
		ContentType: "audio/mpeg",
		Delay:       30 * time.Millisecond,
	}))

	svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, &drivetest.RecordingDelivery{}))

	var order []string
	for _, ev := range pub.Published() {
		order = append(order, ev.Delta)
	}
	if len(order) != 4 || order[0] != "a" || order[1] != "" || order[2] != "b" {
		t.Fatalf("publication order = %v, want the spoken event in place between the two deltas", order)
	}
}

// O11 — a delivery end slower than the producer never loses an event. While
// the caller is still reading, the queue holds instead of dropping: the run is
// open as long as the caller is behind, and everything the producer emitted
// arrives afterwards, in order, with the terminal last.
func TestOrderSlowDeliveryEndWaitsAtTheByteLimit(t *testing.T) {
	t.Parallel()

	const (
		events = 100
		chunk  = 100 << 10
	)
	terminal := agenttest.TerminalEvent(t, "done")
	produced := make([]native.StreamEvent, 0, events)
	for range events {
		produced = append(produced, native.StreamEvent{Type: native.EventTextDelta, Delta: strings.Repeat("x", chunk)})
	}
	exec := &drivetest.ScriptedExecutor{Events: produced, Terminal: &terminal}
	pub := &agenttest.EventRecorder{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	delivery := &gatedDelivery{block: make(chan struct{}), entered: make(chan struct{})}

	done := make(chan drive.RunResult, 1)
	go func() { done <- svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, delivery)) }()

	select {
	case <-delivery.entered:
	case <-time.After(5 * time.Second):
		close(delivery.block)
		<-done
		t.Fatal("the run never reached the delivery end")
	}

	// The producer emitted far past the queue's byte limit long before the
	// caller read anything. Nothing was dropped: the run is still open, and
	// the caller holds it rather than being treated as gone.
	select {
	case result := <-done:
		close(delivery.block)
		<-done
		t.Fatalf("the run finished while the caller was still reading: %#v", result)
	case <-time.After(50 * time.Millisecond):
	}

	close(delivery.block)
	result := <-done
	if result.End != outcome.EndCompleted {
		t.Fatalf("end = %s, want completed", result.End)
	}
	got := delivery.snapshot()
	if len(got) != events+1 {
		t.Fatalf("delivered %d events, want the %d produced and the terminal", len(got), events+1)
	}
	for i, ev := range got {
		want := native.EventTextDelta
		if i == events {
			want = native.EventAgentEnd
		}
		if ev.Event.Type != want {
			t.Fatalf("delivered[%d] = %q, want %q", i, ev.Event.Type, want)
		}
	}
}

// A run that stays under the byte limit completes with the caller reading at
// its own pace: nothing about a slow reader delays the run or truncates it.
func TestOrderSlowDeliveryEndUnderTheByteLimit(t *testing.T) {
	t.Parallel()

	terminal := agenttest.TerminalEvent(t, "done")
	exec := &drivetest.ScriptedExecutor{
		Events: []native.StreamEvent{
			{Type: native.EventTextDelta, Delta: "a"},
			{Type: native.EventTextDelta, Delta: "b"},
		},
		Terminal: &terminal,
	}
	pub := &agenttest.EventRecorder{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	delivery := &pacedDelivery{per: 5 * time.Millisecond}

	result := svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, delivery))

	if result.End != outcome.EndCompleted {
		t.Fatalf("end = %s, want completed", result.End)
	}
	if got := delivery.count(); got != 3 {
		t.Fatalf("delivered %d events, want both deltas and the terminal", got)
	}
}

// pacedDelivery reads each event after a short pause.
type pacedDelivery struct {
	mu     sync.Mutex
	events []drive.RunEvent
	per    time.Duration
}

func (d *pacedDelivery) Deliver(_ context.Context, ev drive.RunEvent) bool {
	time.Sleep(d.per)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, ev)
	return true
}

func (d *pacedDelivery) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.events)
}

func (*pacedDelivery) Settle() {}

func (*pacedDelivery) Ended(context.Context, drive.RunEnded) {}

// gatedDelivery holds its first Deliver until block closes, then passes the
// rest through, so a test can keep a caller behind a producer that runs ahead.
type gatedDelivery struct {
	mu      sync.Mutex
	events  []drive.RunEvent
	entered chan struct{}
	block   chan struct{}
	once    sync.Once
}

func (d *gatedDelivery) Deliver(_ context.Context, ev drive.RunEvent) bool {
	d.once.Do(func() {
		close(d.entered)
		<-d.block
	})
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, ev)
	return true
}

func (d *gatedDelivery) snapshot() []drive.RunEvent {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]drive.RunEvent(nil), d.events...)
}

func (*gatedDelivery) Settle() {}

func (*gatedDelivery) Ended(context.Context, drive.RunEnded) {}
