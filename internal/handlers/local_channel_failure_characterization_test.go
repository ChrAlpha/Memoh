package handlers

// Characterization tests for what the local WebSocket channel reports when an
// Agent run fails. They pin CURRENT behavior so the failure-classification
// refactor can show exactly which outward values it changes. A value that looks
// wrong is still asserted as it is today and marked "current behavior".
//
// The application drives every Web run and names its outcome; the socket is
// told what the run published. Each case hands the initiating socket's
// delivery end the events a run delivers, in their public form, and pins the
// frames it writes. What the run recorded — session_runs, the live view, the
// history rows — is asserted in the application's own tests, where the run is
// driven end to end.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/felinics/memoh/internal/agent/application"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/channel"
)

const (
	failureCharSessionID = "22222222-2222-4222-8222-222222222222"
)

type failureCharRun struct {
	writer   *wsWriter
	ref      wsTurnRef
	delivery *wsRunDelivery
}

func newFailureCharRun(*testing.T) failureCharRun {
	writer := &wsWriter{ch: make(chan []byte, 16), stop: make(chan struct{}), done: make(chan struct{})}
	ref := wsTurnRef{RunID: "33333333-3333-4333-8333-333333333333", SessionID: failureCharSessionID}
	return failureCharRun{writer: writer, ref: ref, delivery: newWSRunDelivery(writer, ref, nil)}
}

// deliver hands the socket's delivery end what a run delivers: each event with
// the public JSON the run published for it.
func (r failureCharRun) deliver(t *testing.T, events ...native.StreamEvent) {
	t.Helper()
	for _, event := range events {
		payload, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		if !r.delivery.Deliver(context.Background(), application.RunEvent{Event: event, Payload: payload}) {
			t.Fatal("the delivery end reported the socket gone")
		}
	}
}

// frames drains every frame written to the initiating socket.
func (r failureCharRun) frames(t *testing.T) []map[string]any {
	t.Helper()
	var frames []map[string]any
	for {
		select {
		case data := <-r.writer.ch:
			var frame map[string]any
			if err := json.Unmarshal(data, &frame); err != nil {
				t.Fatalf("decode frame: %v", err)
			}
			frames = append(frames, frame)
		default:
			return frames
		}
	}
}

func assertFrames(t *testing.T, got []map[string]any, want []map[string]any) {
	t.Helper()
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("socket frames = %s\nwant %s", gotJSON, wantJSON)
	}
}

const (
	providerOverloadedDetail = "The model provider is unavailable or overloaded right now. Please try again in a moment."
	responseInterruptedCopy  = "The model response was interrupted. Please try again."
	runFailedDetail          = "The response could not be completed. Please try again."
)

// Scenarios 2 and 3 on the WebSocket path: the run delivers the public error
// event (the failure already translated to its code) and the agent_abort that
// ends it. Only the error reaches the initiating socket.
func TestCharacterizeWSMidStreamProviderFailure(t *testing.T) {
	t.Parallel()
	r := newFailureCharRun(t)
	r.deliver(t,
		native.StreamEvent{Type: native.EventAgentStart},
		native.StreamEvent{Type: native.EventRetry, Attempt: 1, MaxAttempt: 3},
		native.StreamEvent{Type: native.EventError, Code: "agent.provider_overloaded", Error: providerOverloadedDetail},
		native.StreamEvent{Type: native.EventAgentAbort, Code: "agent.provider_overloaded"},
	)

	assertFrames(t, r.frames(t), []map[string]any{{
		"type": "error", "run_id": r.ref.RunID, "session_id": failureCharSessionID,
		"code": "agent.provider_overloaded", "message": providerOverloadedDetail, "fault": "dependency",
	}})
}

// Scenario 3: a failed attempt that recovers leaves no error anywhere.
func TestCharacterizeWSRetryRecoveredRun(t *testing.T) {
	t.Parallel()
	r := newFailureCharRun(t)
	r.deliver(t,
		native.StreamEvent{Type: native.EventAgentStart},
		native.StreamEvent{Type: native.EventRetry, Attempt: 1, MaxAttempt: 3},
		native.StreamEvent{Type: native.EventTextDelta, Delta: "done"},
		native.StreamEvent{Type: native.EventAgentEnd},
	)

	assertFrames(t, r.frames(t), nil)
}

// Scenario 1 and 10 on the WebSocket path: the run fails before the agent
// stream starts, and it named nothing.
//
// The socket frame carries runtime_run_failed, the code session_runs records,
// with its catalog detail instead of the runtime's own text.
func TestCharacterizeWSPlainRunnerError_CurrentBehavior(t *testing.T) {
	t.Parallel()
	r := newFailureCharRun(t)
	r.deliver(t, native.StreamEvent{Type: native.EventError, Code: "runtime_run_failed", Error: "runtime_run_failed"})

	assertFrames(t, r.frames(t), []map[string]any{{
		"type": "error", "run_id": r.ref.RunID, "session_id": failureCharSessionID,
		"code": "runtime_run_failed", "message": runFailedDetail, "fault": "server",
	}})
}

// Scenario 1 and 10 with a catalogued failure (X1): the frame carries the code
// at the top level with the catalog detail.
func TestCharacterizeWSCodedRunnerError(t *testing.T) {
	t.Parallel()
	r := newFailureCharRun(t)
	r.deliver(t, native.StreamEvent{Type: native.EventError, Code: "workspace.unreachable", Error: "workspace.unreachable"})

	assertFrames(t, r.frames(t), []map[string]any{{
		"type": "error", "run_id": r.ref.RunID, "session_id": failureCharSessionID,
		"code": "workspace.unreachable", "message": "The workspace could not be reached.", "fault": "server",
	}})
}

// Scenario 10: an error the runtime raised without a code. The frame is
// rendered from the event's public form, which names the code the run records;
// the runtime's own text goes nowhere.
func TestCharacterizeWSUncodedStreamError_CurrentBehavior(t *testing.T) {
	t.Parallel()
	r := newFailureCharRun(t)
	raw := native.StreamEvent{Type: native.EventError, Error: "runtime interrupted"}
	public, err := json.Marshal(native.StreamEvent{Type: native.EventError, Code: "agent.response_interrupted", Error: "agent.response_interrupted"})
	if err != nil {
		t.Fatal(err)
	}
	if !r.delivery.Deliver(context.Background(), application.RunEvent{Event: raw, Payload: public}) {
		t.Fatal("the delivery end reported the socket gone")
	}
	assertFrames(t, r.frames(t), []map[string]any{{
		"type": "error", "run_id": r.ref.RunID, "session_id": failureCharSessionID,
		"code": "agent.response_interrupted", "message": responseInterruptedCopy, "fault": "dependency",
	}})

	// An error that reaches the socket with no code at all is sent as
	// runtime_run_failed, the code the run records for it.
	r.deliver(t, raw)
	assertFrames(t, r.frames(t), []map[string]any{{
		"type": "error", "run_id": r.ref.RunID, "session_id": failureCharSessionID,
		"code": "runtime_run_failed", "message": runFailedDetail, "fault": "server",
	}})
}

// Scenario 8 on the WebSocket path: a stopped run ends in agent_abort without
// a failure, and nothing is sent to the socket.
func TestCharacterizeWSCanceledRunnerAfterAbort(t *testing.T) {
	t.Parallel()
	r := newFailureCharRun(t)
	r.deliver(t,
		native.StreamEvent{Type: native.EventAgentStart},
		native.StreamEvent{Type: native.EventAgentAbort},
	)

	assertFrames(t, r.frames(t), nil)
}

// Scenario 5 on the WebSocket path: the admission sentinels become run_rejected
// with a code and its catalog detail; no run is named. Any other admission
// failure is an error frame with the code for its fault.
func TestCharacterizeWSAdmissionRejectionFrames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want map[string]any
	}{
		{
			err: sessionruntime.ErrSessionBusy,
			want: map[string]any{
				"type": "run_rejected", "invocation_id": "invocation-1", "session_id": failureCharSessionID,
				"code": "session_runtime.session_busy", "message": "This conversation is still working on the previous message. Please try again shortly.",
			},
		},
		{
			// Ownership loss during admission is not a rejection sentinel. It is
			// this process's failure, so the client gets internal.
			err: sessionruntime.ErrRunOwnershipLost,
			want: map[string]any{
				"type": "error", "invocation_id": "invocation-1", "session_id": failureCharSessionID,
				"code": "internal", "message": "Something went wrong on the server. Please try again.", "fault": "server",
			},
		},
	} {
		writer := &wsWriter{ch: make(chan []byte, 1), stop: make(chan struct{}), done: make(chan struct{})}
		_ = sendWSErrorFromError(context.Background(), writer, wsTurn("invocation-1", failureCharSessionID), tc.err)
		var frame map[string]any
		if err := json.Unmarshal(<-writer.ch, &frame); err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		assertFrames(t, []map[string]any{frame}, []map[string]any{tc.want})
	}
}

// SSE exit: the local channel's SSE stream carries the IM processor's error
// event (see the inbound characterization tests for its text).
//
// Current behavior: the SSE error event has only the text, no code.
func TestCharacterizeSSEErrorEventHasNoCode_CurrentBehavior(t *testing.T) {
	t.Parallel()
	data, err := formatLocalStreamEvent(channel.StreamEvent{Type: channel.StreamEventError, Error: providerOverloadedDetail})
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	if want := `{"type":"error","error":"` + providerOverloadedDetail + `"}`; string(data) != want {
		t.Fatalf("sse event = %s, want %s", data, want)
	}
}
