package drive_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/drive/drivetest"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	chatview "github.com/felinics/memoh/internal/agent/view"
	"github.com/felinics/memoh/internal/apperror"
)

// recordingDecisionSink records every write a decision publisher makes, in the
// order it made them, and can refuse a chosen write.
type recordingDecisionSink struct {
	mu     sync.Mutex
	writes []string
	// projectionErrAt and outputErrAt refuse the k-th (1-based) write of that
	// kind; a nil payload counts as an output write.
	projectionErrAt, outputErrAt int
	projections, outputs         int
	seqs                         []int64
}

var errSinkRefused = errors.New("sink refused")

func (s *recordingDecisionSink) HandleAgentEvent(_ context.Context, _ sessionruntime.RunHandle, ev native.StreamEvent) ([]chatview.UIMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projections++
	if s.projections == s.projectionErrAt {
		return nil, errSinkRefused
	}
	s.writes = append(s.writes, "projection:"+string(ev.Type))
	return nil, nil
}

func (s *recordingDecisionSink) PublishDecisionOutput(_ context.Context, _ sessionruntime.Command, seq int64, payload json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outputs++
	if s.outputs == s.outputErrAt {
		return errSinkRefused
	}
	kind := "close"
	if payload != nil {
		var ev native.StreamEvent
		_ = json.Unmarshal(payload, &ev)
		kind = string(ev.Type)
	}
	s.writes = append(s.writes, "output:"+kind)
	s.seqs = append(s.seqs, seq)
	return nil
}

func (s *recordingDecisionSink) snapshot() ([]string, []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.writes...), append([]int64(nil), s.seqs...)
}

// decisionSpec is a routed continuation's spec over sink: an admitted run
// whose events go to the projection and the answering command's output.
func decisionSpec(exec drive.RunExecutor, sink drive.DecisionOutputSink) drive.RunSpec {
	spec := drivetest.ScheduledSpec(exec, drive.NoDelivery{})
	spec.Input = outcome.InputAccepted
	spec.Unattended = false
	spec.Publisher = drive.NewDecisionOutputPublisher(sink, spec.Run.Handle, sessionruntime.Command{ID: "answer", StreamOutput: true})
	return spec
}

// Each event reaches the projection before the output, every output write
// takes the next sequence, and the output is closed after the last event.
func TestDecisionPublisherWritesProjectionThenOutputInOrder(t *testing.T) {
	t.Parallel()

	terminal := native.StreamEvent{Type: native.EventAgentEnd}
	exec := &drivetest.ScriptedExecutor{
		Events:   []native.StreamEvent{{Type: native.EventAgentStart}, {Type: native.EventTextDelta, Delta: "answer"}},
		Terminal: &terminal,
	}
	sink := &recordingDecisionSink{}
	svc := newDriveService(nil, &agenttest.ScriptedAdmitter{Started: true})

	result := svc.Drive(context.Background(), decisionSpec(exec, sink))

	if result.End != outcome.EndCompleted {
		t.Fatalf("end = %s, want completed (failure %#v)", result.End, result.Failure)
	}
	writes, seqs := sink.snapshot()
	want := []string{
		"projection:agent_start", "output:agent_start",
		"projection:text_delta", "output:text_delta",
		"projection:agent_end", "output:agent_end",
		"output:close",
	}
	if len(writes) != len(want) {
		t.Fatalf("writes = %v, want %v", writes, want)
	}
	for i := range want {
		if writes[i] != want[i] {
			t.Fatalf("writes = %v, want %v", writes, want)
		}
	}
	for i, seq := range seqs {
		if seq != int64(i+1) {
			t.Fatalf("output sequences = %v, want 1..%d in order", seqs, len(seqs))
		}
	}
}

// Either write refused fails the run under the publication code, and the
// refused publisher is not written to again: not for the remaining events,
// not for the terminal, and not to close.
func TestDecisionPublisherEitherRefusalFailsTheRun(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		sink *recordingDecisionSink
		want []string
	}{
		{"projection", &recordingDecisionSink{projectionErrAt: 2}, []string{"projection:agent_start", "output:agent_start"}},
		{"output", &recordingDecisionSink{outputErrAt: 2}, []string{"projection:agent_start", "output:agent_start", "projection:text_delta"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			terminal := native.StreamEvent{Type: native.EventAgentEnd}
			exec := &drivetest.ScriptedExecutor{
				Events:   []native.StreamEvent{{Type: native.EventAgentStart}, {Type: native.EventTextDelta, Delta: "a"}, {Type: native.EventTextDelta, Delta: "b"}},
				Terminal: &terminal,
			}
			admitter := &agenttest.ScriptedAdmitter{Started: true}
			svc := newDriveService(nil, admitter)

			result := svc.Drive(context.Background(), decisionSpec(exec, tc.sink))

			if result.End != outcome.EndFailed || result.Failure == nil || result.Failure.Code != apperror.CodeSessionPublishFailed {
				t.Fatalf("result = %s %#v, want failed %s", result.End, result.Failure, apperror.CodeSessionPublishFailed)
			}
			writes, _ := tc.sink.snapshot()
			if len(writes) != len(tc.want) {
				t.Fatalf("writes = %v, want %v and nothing after the refusal", writes, tc.want)
			}
			for i := range tc.want {
				if writes[i] != tc.want[i] {
					t.Fatalf("writes = %v, want %v", writes, tc.want)
				}
			}
			if len(admitter.Finishes) != 1 || admitter.Finishes[0].Status != sessionruntime.RunStatusErrored ||
				admitter.Finishes[0].Message != string(apperror.CodeSessionPublishFailed) {
				t.Fatalf("terminal writes = %#v, want one errored %s", admitter.Finishes, apperror.CodeSessionPublishFailed)
			}
		})
	}
}

// A parked run whose output cannot be closed does not stay parked: a reader
// would wait on that output for an answer nobody can give it, so the refusal
// ends the run like any other.
func TestDecisionPublisherCloseRefusalEndsAParkedRun(t *testing.T) {
	t.Parallel()

	terminal := native.StreamEvent{Type: native.EventAgentEnd, ApprovalID: "next", Status: "pending"}
	exec := &drivetest.ScriptedExecutor{
		Events:   []native.StreamEvent{{Type: native.EventToolApprovalRequest, ApprovalID: "next", Status: "pending"}},
		Terminal: &terminal,
		Deferred: true,
	}
	// The request and the terminal are appended; the third output write is
	// the close.
	sink := &recordingDecisionSink{outputErrAt: 3}
	admitter := &agenttest.ScriptedAdmitter{Started: true}
	svc := newDriveService(nil, admitter)

	result := svc.Drive(context.Background(), decisionSpec(exec, sink))

	if result.End != outcome.EndFailed || result.Failure == nil || result.Failure.Code != apperror.CodeSessionPublishFailed {
		t.Fatalf("result = %s %#v, want failed %s", result.End, result.Failure, apperror.CodeSessionPublishFailed)
	}
	if writes, _ := sink.snapshot(); len(writes) != 4 || writes[3] != "output:agent_end" {
		t.Fatalf("writes = %v, want everything up to the refused close", writes)
	}
	if len(admitter.Finishes) != 1 || admitter.Finishes[0].Status != sessionruntime.RunStatusErrored {
		t.Fatalf("terminal writes = %#v, want the parked run ended errored", admitter.Finishes)
	}
}
