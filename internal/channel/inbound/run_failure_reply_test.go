package inbound

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/i18n"
)

// terminalEvent builds the run_terminal event a run's failure is answered from.
func terminalEvent(t *testing.T, terminal turn.RunTerminal) turn.Event {
	t.Helper()
	payload, err := json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	return turn.Event{Kind: turn.EventRunTerminal, Payload: payload}
}

// replyOf runs the failure reply for one terminal event and returns the error
// event it renders.
func replyOf(t *testing.T, terminal turn.RunTerminal, turnErr error) (channel.StreamEvent, bool) {
	t.Helper()
	var r runFailureReply
	r.reportsTerminal = true
	if !r.observeTerminal(terminalEvent(t, terminal)) {
		t.Fatal("run_terminal was not recognized")
	}
	return r.reply(localizerForTest(t), turnErr)
}

func localizerForTest(t *testing.T) *i18n.Localizer {
	t.Helper()
	return i18n.New("en")
}

// The failure reply is rendered from the run_terminal's own args: they are what
// the run's record carries, and the turn error may not survive the process
// boundary between the two.
func TestRunFailureReplyRendersTheTerminalsArgs(t *testing.T) {
	event, ok := replyOf(t, turn.RunTerminal{
		Type:      turn.EventRunTerminal,
		State:     turn.RunStateFailed,
		ErrorCode: string(apperror.CodeAgentProviderAuthFailed),
		ErrorArgs: map[string]string{"provider": "acme"},
	}, nil)
	if !ok {
		t.Fatal("no reply for a failed run_terminal")
	}
	if event.Type != channel.StreamEventError {
		t.Fatalf("event type = %q, want %q", event.Type, channel.StreamEventError)
	}
}

// A terminal that carries no args falls back to the turn error when it names
// the same code, so a run whose record dropped its args still answers with
// them.
func TestRunFailureReplyFallsBackToTheTurnErrorArgs(t *testing.T) {
	turnErr := apperror.Wrap(apperror.CodeSessionHistoryInconsistent, errors.New("private"), map[string]string{"phase": "commit"})
	event, ok := replyOf(t, turn.RunTerminal{
		Type:      turn.EventRunTerminal,
		State:     turn.RunStateFailed,
		ErrorCode: string(apperror.CodeSessionHistoryInconsistent),
	}, turnErr)
	if !ok {
		t.Fatal("no reply for a failed run_terminal")
	}
	if event.Type != channel.StreamEventError {
		t.Fatalf("event type = %q, want %q", event.Type, channel.StreamEventError)
	}
}

// A terminal whose code differs from the turn error's is answered by its own
// code alone: the two describe different failures, and the args belong to the
// error that produced them.
func TestRunFailureReplyDoesNotBorrowArgsFromAnotherCode(t *testing.T) {
	turnErr := apperror.Wrap(apperror.CodeSessionHistoryInconsistent, errors.New("private"), map[string]string{"phase": "commit"})
	event, ok := replyOf(t, turn.RunTerminal{
		Type:      turn.EventRunTerminal,
		State:     turn.RunStateFailed,
		ErrorCode: string(apperror.CodeAgentProviderAuthFailed),
	}, turnErr)
	if !ok {
		t.Fatal("no reply for a failed run_terminal")
	}
	if event.Type != channel.StreamEventError {
		t.Fatalf("event type = %q, want %q", event.Type, channel.StreamEventError)
	}
}

// A run that did not fail is not replied to from its terminal, even when it
// reported an error along the way: the error event the run delivered is what
// answers for it.
func TestRunFailureReplySkipsASuccessfulTerminal(t *testing.T) {
	if _, ok := replyOf(t, turn.RunTerminal{Type: turn.EventRunTerminal, State: turn.RunStateCompleted}, nil); ok {
		t.Fatal("a completed run_terminal produced a failure reply")
	}
}

// A stop is not a failure: the caller that ended the run already knows, and the
// platform is not told the turn broke.
func TestRunFailureReplySkipsAStoppedRun(t *testing.T) {
	if _, ok := replyOf(t, turn.RunTerminal{Type: turn.EventRunTerminal, State: turn.RunStateAborted}, nil); ok {
		t.Fatal("an aborted run_terminal produced a failure reply")
	}
}
