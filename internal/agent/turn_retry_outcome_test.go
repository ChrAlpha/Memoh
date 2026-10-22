package agent

import (
	"net/http"
	"testing"

	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
)

// X4, turn RPC (IM) native path: a stream error that the runtime retried and
// recovered from is not reported. The stream carries the retry and no error,
// the caller's Errs() stays empty, both finish proposals say completed, and
// session_runs ends completed.
func TestChatTurnRetrySuccessReportsNoError(t *testing.T) {
	fixture, transport := newScriptedFailureFixture(t, nil, nil, http.StatusServiceUnavailable)
	got := runNativeTurn(t, fixture)

	if calls := transport.calls.Load(); calls < 2 {
		t.Fatalf("model requests = %d, want the failed attempt and its retry", calls)
	}
	assertOrdered(t, got.types, "retry", "agent_end")
	assertCount(t, got.types, "error", 0)
	if len(fixture.messages.Persisted) == 0 {
		t.Fatal("the recovered answer was not stored")
	}
	assertStrings(t, "turn error codes", got.errCodes, nil)
	// The AgentEnd event proposes completed through HandleAgentEvent, and the
	// run's terminal write proposes the same outcome afterwards.
	wantProposals := [][2]string{{"completed", ""}, {"completed", ""}}
	if len(got.proposals) != len(wantProposals) || got.proposals[0] != wantProposals[0] || got.proposals[1] != wantProposals[1] {
		t.Fatalf("finish proposals = %q, want %q", got.proposals, wantProposals)
	}
	if want := [3]string{"completed", "", ""}; got.ledger != want {
		t.Fatalf("session_runs = %q, want %q", got.ledger, want)
	}
}

// A retry that does not recover still fails the turn: the retried 503 is
// followed by a non-retryable 400, the run ends with agent_abort, and the
// caller's Errs() reports it.
func TestChatTurnRetryFailureReportsError(t *testing.T) {
	fixture, transport := newScriptedFailureFixture(t, nil, nil, http.StatusServiceUnavailable, http.StatusBadRequest)
	got := runNativeTurn(t, fixture)

	if calls := transport.calls.Load(); calls != 2 {
		t.Fatalf("model requests = %d, want the retried attempt and the final one", calls)
	}
	assertOrdered(t, got.types, "retry", "error", "agent_abort")
	assertCount(t, got.types, "error", 1)
	// The 400 the provider answered last names the run's failure.
	assertStrings(t, "turn error codes", got.errCodes, []string{string(apperror.CodeAgentProviderRequestRejected)})
	if want := [3]string{"failed", got.errCodes[0], ""}; got.ledger != want {
		t.Fatalf("session_runs = %q, want %q", got.ledger, want)
	}
}

// assertCount checks that value appears in got exactly want times.
func assertCount(t *testing.T, got []string, value string, want int) {
	t.Helper()
	count := 0
	for _, item := range got {
		if item == value {
			count++
		}
	}
	if count != want {
		t.Fatalf("event types = %q, want %d %q", got, want, value)
	}
}

// When the terminal event's proposal write fails, the run's outcome is deferred
// to finish. The turn port's finish already holds the delivered failure, so the
// row still records the code the caller was told about.
func TestChatTurnDeferredProposalFinishKeepsDeliveredCode(t *testing.T) {
	runs := &proposalRecordingLedger{Store: sessionledger.New(), failFirst: true}
	fixture, _ := newScriptedFailureFixture(t, runs, nil, http.StatusServiceUnavailable, http.StatusBadRequest)
	got := runNativeTurn(t, fixture)

	assertOrdered(t, got.types, "retry", "error", "agent_abort")
	if len(got.errCodes) != 1 || got.errCodes[0] == "" {
		t.Fatalf("turn error codes = %q, want exactly one coded error", got.errCodes)
	}
	if len(got.proposals) != 2 || got.proposals[1] != [2]string{"failed", got.errCodes[0]} {
		t.Fatalf("finish proposals = %q, want the deferred one from finish with %q", got.proposals, got.errCodes[0])
	}
	if want := [3]string{"failed", got.errCodes[0], ""}; got.ledger != want {
		t.Fatalf("session_runs = %q, want %q", got.ledger, want)
	}
}
