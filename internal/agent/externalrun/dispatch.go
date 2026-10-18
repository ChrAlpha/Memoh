package externalrun

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/apperror"
	session "github.com/felinics/memoh/internal/chat/thread"
)

// DispatchKind names where a request's turn runs.
type DispatchKind int

const (
	// DispatchNative runs the turn on the in-process model runtime.
	DispatchNative DispatchKind = iota
	// DispatchExternal runs the turn on an external runtime driver.
	DispatchExternal
)

// Dispatch is the resolved execution route for one chat request.
type Dispatch struct {
	Kind   DispatchKind
	Driver external.Driver
}

// ResolveDispatch decides which runtime executes a request's turn:
// the in-process model runtime, the ACP pool, or a direct external driver.
func (h *Host) ResolveDispatch(ctx context.Context, req runreq.ChatRequest) (Dispatch, error) {
	if strings.TrimSpace(req.ThreadID) == "" {
		return Dispatch{Kind: DispatchNative}, nil
	}
	sess, err := h.sessions.Get(ctx, req.ThreadID)
	if err != nil {
		return Dispatch{}, err
	}
	if err := runreq.ValidateSessionBot(req.BotID, req.ThreadID, sess.BotID); err != nil {
		return Dispatch{}, err
	}
	runtimeType := ""
	switch {
	case session.IsACPRuntime(sess):
		runtimeType = session.RuntimeACPAgent
	case session.IsDirectRuntime(sess):
		runtimeType = strings.TrimSpace(sess.RuntimeType)
	default:
		return Dispatch{Kind: DispatchNative}, nil
	}
	driver, ok := h.drivers[runtimeType]
	if !ok {
		return Dispatch{}, apperror.New(apperror.CodeExternalRuntimeUnavailable, map[string]string{"runtime": runtimeType})
	}
	return Dispatch{Kind: DispatchExternal, Driver: driver}, nil
}

// abortSettleWait bounds how long AbortSessionRuns waits for the aborted run
// to reach a terminal state. It must outlast the drivers' own interrupt
// grace windows (codex settle + recycle, claude interrupt grace) so a
// successful return means the runtime has actually stopped.
const abortSettleWait = 30 * time.Second

// AbortSessionRuns interrupts the session's active run, if any, and waits —
// bounded — until it actually stops. Deleting or resetting a session on a
// direct external runtime must not leave its turn executing (and mutating
// the workspace) behind the deletion; cancel alone only signals the driver,
// so callers that destroy the session next need the settle, not the signal.
// A run that cannot settle inside the window fails the call rather than
// letting the caller proceed over a still-running turn.
func (h *Host) AbortSessionRuns(ctx context.Context, botID, sessionID string) error {
	snapshot, err := h.runtime.Snapshot(ctx, botID, sessionID)
	if err != nil {
		return err
	}
	run := snapshot.CurrentRunView
	if run == nil || strings.TrimSpace(run.RunID) == "" {
		return nil
	}
	runID := run.RunID
	if _, err = h.runtime.Abort(ctx, botID, sessionID, runID); err != nil {
		return err
	}
	deadline := time.Now().Add(abortSettleWait)
	for {
		snapshot, err := h.runtime.Snapshot(ctx, botID, sessionID)
		if err != nil {
			return err
		}
		current := snapshot.CurrentRunView
		// The ledger going terminal is not enough: a waiting_decision run is
		// stamped aborted the moment its cancel is dispatched, while the
		// driver's interrupt handshake is still tearing the turn down. The
		// active-prompt registration lives exactly as long as the driver
		// call, so wait for both.
		if (current == nil || current.RunID != runID || !activeRuntimeRunStatus(current.Status)) &&
			!h.prompts.Has(botID, sessionID) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("aborted run %s did not stop within %s", runID, abortSettleWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func activeRuntimeRunStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case sessionruntime.RunStatusAdmitting, sessionruntime.RunStatusRunning,
		sessionruntime.RunStatusWaitingDecision, sessionruntime.RunStatusAborting:
		return true
	default:
		return false
	}
}

// handleRuntimeRoundRollback resolves a definite round rollback: a driver
// that repairs its runtime state (the ACP pool discards the warm process so
// the next prompt restarts from the durable head) gets the callback for
// completed turns; every other case keeps the runtime session and logs the
// ghost turn.
func (h *Host) handleRuntimeRoundRollback(ctx context.Context, driver external.Driver, req runreq.ChatRequest, runtimeType string, turnCompleted bool) {
	if turnCompleted {
		if handler, ok := driver.(external.RoundRollbackHandler); ok {
			handler.OnRoundRolledBack(context.WithoutCancel(ctx), req.BotID, req.ThreadID)
			return
		}
	}
	h.logRuntimeRoundDivergence(req, runtimeType)
}

func (h *Host) logRuntimeRoundDivergence(req runreq.ChatRequest, runtimeType string) {
	h.logger.Warn("external runtime remembers a round the visible history lost",
		slog.String("session_id", req.ThreadID),
		slog.String("run_id", req.RunID),
		slog.String("runtime", runtimeType),
	)
}

// runtimeTurnRan reports evidence that the runtime accepted the turn. "Nothing
// ran" is otherwise only a driver's promise about its error codes, and a wrong
// code must cost a stored failure round, never the user's message. A publication
// request is no evidence: ACP reports it on every result.
func runtimeTurnRan(result external.PromptResult) bool {
	return strings.TrimSpace(result.AgentTurnID) != "" || len(result.Output) > 0
}

func runtimeTerminalStreamEvent(eventType native.StreamEventType, result external.PromptResult) native.StreamEvent {
	ev := native.StreamEvent{Type: eventType}
	if len(result.Notices) > 0 {
		ev.Metadata = map[string]any{event.RuntimeNoticesMetadataKey: result.Notices}
	}
	if data, err := json.Marshal(result.Output); err == nil {
		ev.Messages = data
	}
	if result.Usage != nil {
		if data, err := json.Marshal(result.Usage); err == nil {
			ev.Usage = data
		}
	}
	return ev
}
