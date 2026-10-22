package outcome

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/errs"
)

// The error event that reports a coded failure carries the catalog's public
// args and nothing from the cause or from args the catalog does not allow.
func TestAgentFailureStreamEventCarriesPublicArgs(t *testing.T) {
	t.Parallel()

	cause := apperror.Wrap(apperror.CodeAgentDependencyMissing, errors.New("SECRET"), map[string]string{"dep_id": "python", "secret": "SECRET"})
	ev := AgentFailureStreamEvent(cause)

	if ev.Code != string(apperror.CodeAgentDependencyMissing) || len(ev.Args) != 1 || ev.Args["dep_id"] != "python" {
		t.Fatalf("failure event = %+v, want %s with dep_id only", ev, apperror.CodeAgentDependencyMissing)
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"args":{"dep_id":"python"}`) || strings.Contains(string(raw), "SECRET") {
		t.Fatalf("failure event wire = %s", raw)
	}
}

// The event as it leaves the application keeps its public args and drops the
// cause, so a subscriber reads the same failure the run recorded.
func TestPublicAgentStreamEventKeepsPublicArgs(t *testing.T) {
	t.Parallel()

	cause := apperror.Wrap(apperror.CodeAgentDependencyMissing, errors.New("SECRET"), map[string]string{"dep_id": "python"})
	ev := PublicAgentStreamEvent(native.StreamEvent{Type: native.EventError, Code: string(apperror.CodeAgentDependencyMissing), Args: map[string]string{"dep_id": "python"}, Cause: cause})

	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Code != string(apperror.CodeAgentDependencyMissing) || ev.Args["dep_id"] != "python" || strings.Contains(string(raw), "SECRET") {
		t.Fatalf("published failure = %s, want %s with dep_id and no cause", raw, apperror.CodeAgentDependencyMissing)
	}
}

// A failure handed to the delivery end is a public error with the run's code,
// marked as already recorded: the outer record of it is then a WARN rather than
// a second ERROR.
func TestRunFailureRecordedIsMarkedRecorded(t *testing.T) {
	t.Parallel()

	failure := RunFailure{Code: apperror.CodeAgentProviderOverloaded, Cause: errors.New("upstream 503"), Phase: PhaseExecution}
	err := failure.Recorded()
	if apperror.CodeOf(err) != apperror.CodeAgentProviderOverloaded {
		t.Fatalf("Recorded() = %v, want the run's code", err)
	}
	if !errs.Analyze(context.Background(), err).Recorded {
		t.Fatalf("Recorded() = %v, want it marked recorded", err)
	}
}

// The cause a stopped run is reported with is the one its context was
// cancelled with: a caller's own cause comes back unchanged and is never
// rewritten as a response timeout.
func TestAgentAbortCauseKeepsTheCallersCause(t *testing.T) {
	t.Parallel()

	custom := errors.New("caller gave up")
	for name, cause := range map[string]error{"stop marker": ErrRunStopped, "caller's own cause": custom} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(cause)
			got := AgentAbortCause(ctx)
			if !errors.Is(got, cause) {
				t.Fatalf("AgentAbortCause = %v, want the cancellation's own cause", got)
			}
			if apperror.CodeOf(got) == apperror.CodeAgentResponseTimeout {
				t.Fatalf("AgentAbortCause = %v, rewritten as a response timeout", got)
			}
		})
	}
}
