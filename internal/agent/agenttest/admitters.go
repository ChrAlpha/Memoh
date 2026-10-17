package agenttest

import (
	"context"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

// RecordedFinish is one finish call an admitter double received.
type RecordedFinish struct {
	Handle  sessionruntime.RunHandle
	Status  string
	Message string
	Args    map[string]string
}

// terminalStateForStatus names the durable state a live status ends as.
func terminalStateForStatus(status string) string {
	switch status {
	case sessionruntime.RunStatusCompleted:
		return "completed"
	case sessionruntime.RunStatusAborted:
		return "aborted"
	case sessionruntime.RunStatusErrored:
		return "failed"
	default:
		return ""
	}
}

// NewScriptedAdmitter returns an admitter that accepts every turn.
func NewScriptedAdmitter() *ScriptedAdmitter {
	return &ScriptedAdmitter{Started: true}
}

// ScriptedAdmitter stands in for the session runtime so these tests can exercise
// what this package owns: the translation of an admission answer into the turn
// port's vocabulary, and the terminal write a finished run must produce. The
// answers themselves are scripted because their correctness is established where
// admission is implemented, not here.
type ScriptedAdmitter struct {
	Mu sync.Mutex

	// admitErr, when set, is what Admit returns instead of an admission.
	AdmitErr error
	// started reports whether the admission owns execution. False models a
	// replay of a run owned elsewhere or already finished.
	Started bool

	// terminal and finishErr are what FinishRunWithErrorCode returns.
	Terminal  sessionruntime.TerminalRun
	FinishErr error

	Inputs     []sessionruntime.AdmitInput
	Finishes   []RecordedFinish
	published  []native.StreamEvent
	PublishErr error

	// finishStarted and finishRelease, when non-nil, hold the terminal write
	// open: a test observes the run in its finishing window, between the last
	// event and the durable record.
	FinishStarted chan struct{}
	FinishRelease chan struct{}
}

func (a *ScriptedAdmitter) Admit(_ context.Context, in sessionruntime.AdmitInput) (sessionruntime.Admission, error) {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	a.Inputs = append(a.Inputs, in)
	if a.AdmitErr != nil {
		return sessionruntime.Admission{}, a.AdmitErr
	}
	runID := "run-" + strconv.Itoa(len(a.Inputs))
	if !a.Started {
		return sessionruntime.Admission{RunID: runID, Replay: true}, nil
	}
	return sessionruntime.Admission{
		RunID:        runID,
		TurnID:       "turn-" + strconv.Itoa(len(a.Inputs)),
		TurnPosition: int64(len(a.Inputs) - 1),
		Started:      true,
		Handle: sessionruntime.RunHandle{
			BotID:        in.BotID,
			SessionID:    in.SessionID,
			RunID:        runID,
			FencingToken: int64(len(a.Inputs)),
		},
	}, nil
}

func (a *ScriptedAdmitter) FinishRunWithErrorCode(ctx context.Context, handle sessionruntime.RunHandle, status, message string) (sessionruntime.TerminalRun, error) {
	return a.FinishRunWithFailure(ctx, handle, status, message, nil)
}

func (a *ScriptedAdmitter) FinishRunWithFailure(_ context.Context, handle sessionruntime.RunHandle, status, message string, args map[string]string) (sessionruntime.TerminalRun, error) {
	if a.FinishStarted != nil {
		close(a.FinishStarted)
	}
	if a.FinishRelease != nil {
		<-a.FinishRelease
	}
	a.Mu.Lock()
	defer a.Mu.Unlock()
	a.Finishes = append(a.Finishes, RecordedFinish{Handle: handle, Status: status, Message: message, Args: args})
	if a.FinishErr != nil {
		return a.Terminal, a.FinishErr
	}
	if a.Terminal.RunID != "" {
		return a.Terminal, nil
	}
	// A real write records the run it ended; the double reports the same, so a
	// caller that reads the record sees what it wrote.
	return sessionruntime.TerminalRun{
		RunID:        handle.RunID,
		BotID:        handle.BotID,
		SessionID:    handle.SessionID,
		FencingToken: handle.FencingToken,
		State:        terminalStateForStatus(status),
		ErrorCode:    message,
		ErrorArgs:    args,
		Applied:      true,
	}, nil
}

func (*ScriptedAdmitter) MarkInlineDecisionRun(string, string, string) {}

func (a *ScriptedAdmitter) PublishAgentEvent(
	_ context.Context,
	_ sessionruntime.RunHandle,
	event native.StreamEvent,
) error {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	a.published = append(a.published, event)
	return a.PublishErr
}

func (a *ScriptedAdmitter) Admitted() []sessionruntime.AdmitInput {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	return slices.Clone(a.Inputs)
}

// publishedEvents returns what the run published to the session projection.
func (a *ScriptedAdmitter) PublishedEvents() []native.StreamEvent {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	return slices.Clone(a.published)
}

// awaitFinish waits for the run's terminal write, which happens in the pump's
// defer stack and so can trail the closing of the handle's channels.
func (a *ScriptedAdmitter) AwaitFinish(t *testing.T) RecordedFinish {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.Mu.Lock()
		finishes := slices.Clone(a.Finishes)
		a.Mu.Unlock()
		if len(finishes) > 0 {
			return finishes[len(finishes)-1]
		}
		if time.Now().After(deadline) {
			t.Fatal("run ended without a terminal write: the thread's slot is never released")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// LifecycleAdmitter is a turn admitter double that records how runs finish.
type LifecycleAdmitter struct {
	Admission sessionruntime.Admission
	Cancel    context.CancelFunc
	Finishes  []RecordedFinish
	FinishErr error
}

func (a *LifecycleAdmitter) Admit(
	_ context.Context,
	input sessionruntime.AdmitInput,
) (sessionruntime.Admission, error) {
	a.Cancel = input.Execution.Cancel
	return a.Admission, nil
}

func (a *LifecycleAdmitter) FinishRunWithErrorCode(ctx context.Context, handle sessionruntime.RunHandle, status, code string) (sessionruntime.TerminalRun, error) {
	return a.FinishRunWithFailure(ctx, handle, status, code, nil)
}

func (a *LifecycleAdmitter) FinishRunWithFailure(
	_ context.Context,
	handle sessionruntime.RunHandle,
	status, code string,
	args map[string]string,
) (sessionruntime.TerminalRun, error) {
	a.Finishes = append(a.Finishes, RecordedFinish{Handle: handle, Status: status, Message: code, Args: args})
	return sessionruntime.TerminalRun{}, a.FinishErr
}

func (*LifecycleAdmitter) MarkInlineDecisionRun(string, string, string) {}
