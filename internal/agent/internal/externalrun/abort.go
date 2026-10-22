package externalrun

import (
	"context"

	"github.com/felinics/memoh/internal/agent/internal/lifecycle"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
)

// AbortRuntimeRun routes an abort through the durable runtime and then
// reconciles its lifecycle asynchronously. The acknowledgement remains owned
// by AbortControl; audit persistence failures are counted and logged only.
//
//nolint:contextcheck // reconciliation intentionally outlives the acknowledgement request.
func (h *Host) AbortRuntimeRun(
	ctx context.Context,
	botID, sessionID, runID, controlID string,
) (bool, error) {
	ctx = runreq.NonNilContext(ctx)
	applied, err := h.aborter.AbortControl(ctx, botID, sessionID, runID, controlID)
	if applied || err != nil {
		reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lifecycle.WriteTimeout)
		go func() {
			defer cancel()
			h.lifecycle.ReconcileAborted(reconcileCtx, runID, botID, sessionID)
		}()
	}
	return applied, err
}

// AbortCurrentRun aborts the run in flight on a session and reports whether
// one was there to abort.
func (h *Host) AbortCurrentRun(ctx context.Context, botID, sessionID string) (bool, error) {
	snapshot, err := h.runtime.Snapshot(ctx, botID, sessionID)
	if err != nil {
		return false, err
	}
	if snapshot.CurrentRunView == nil {
		return false, nil
	}
	return h.AbortRuntimeRun(ctx, botID, sessionID, snapshot.CurrentRunView.RunID, "")
}
