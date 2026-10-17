package drive

import (
	"context"
	"errors"
	"testing"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/apperror"
)

// A clean terminal clears an earlier retryable error: a retried failure that
// recovered is not the run's outcome.
func TestRunObserverCleanTerminalClearsAnEarlierError(t *testing.T) {
	t.Parallel()

	driver := New(Deps{})
	obs := newRunObserver()
	assets := newOutboundAssets()
	retryErr := apperror.Wrap(apperror.CodeAgentProviderOverloaded, errors.New("503"), nil)
	obs.observe(context.Background(), driver, "bot-1", assets, false, native.StreamEvent{Type: native.EventError, Cause: retryErr})
	obs.observe(context.Background(), driver, "bot-1", assets, false, native.StreamEvent{Type: native.EventAgentEnd})

	if obs.declared != nil {
		t.Fatalf("declared = %v, want a recovered retry to clear it", obs.declared)
	}
}

// The observer's declared failure is the first one, whichever entry declared
// it: a later failure is a consequence, not the run's name.
func TestRunObserverKeepsTheFirstDeclaredFailure(t *testing.T) {
	t.Parallel()

	obs := newRunObserver()
	first := apperror.Wrap(apperror.CodeAgentProviderAuthFailed, errors.New("401"), nil)
	obs.declare(first, false)
	obs.declare(errors.New("later diagnostic"), false)

	if !errors.Is(obs.declared, first) {
		t.Fatalf("declared = %v, want the first failure to keep its name", obs.declared)
	}
}
