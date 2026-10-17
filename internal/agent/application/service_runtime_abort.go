package application

import (
	"context"
	"errors"

	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/runreq"
)

type runtimeAbortController interface {
	AbortControl(ctx context.Context, botID, sessionID, runID, controlID string) (bool, error)
}

// AbortRuntimeRun routes an abort through the durable runtime and then
// reconciles its lifecycle asynchronously. The acknowledgement remains owned
// by AbortControl; audit persistence failures are counted and logged only.
//
//nolint:contextcheck // reconciliation intentionally outlives the acknowledgement request.
func (s *Service) AbortRuntimeRun(
	ctx context.Context,
	botID, sessionID, runID, controlID string,
) (bool, error) {
	if s == nil || s.abortRuntime == nil {
		return false, errors.New("session runtime abort is not configured")
	}
	ctx = runreq.NonNilContext(ctx)
	applied, err := s.abortRuntime.AbortControl(ctx, botID, sessionID, runID, controlID)
	if applied || err != nil {
		reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lifecycle.WriteTimeout)
		go func() {
			defer cancel()
			s.lifecycle.ReconcileAborted(reconcileCtx, runID, botID, sessionID)
		}()
	}
	return applied, err
}
