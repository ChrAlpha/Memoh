package externalrun

import (
	"context"

	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/runreq"
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
