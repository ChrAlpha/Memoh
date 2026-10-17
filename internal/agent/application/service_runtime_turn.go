package application

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/apperror"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/errs"
)

// externalDriverMap indexes out-of-process runtime drivers (codex,
// claude-code, the ACP pool's adapter) by runtime type; a later driver replaces
// an earlier one of the same type. Sessions whose runtime type has no driver
// fail with a stable "runtime unavailable" error instead of silently degrading
// to the built-in model runtime.
func externalDriverMap(drivers ...external.Driver) map[string]external.Driver {
	byType := map[string]external.Driver{}
	for _, driver := range drivers {
		if driver == nil {
			continue
		}
		byType[strings.TrimSpace(driver.RuntimeType())] = driver
	}
	return byType
}

type runtimeDispatchKind int

const (
	dispatchNative runtimeDispatchKind = iota
	dispatchExternal
)

// runtimeDispatch is the resolved execution route for one chat request.
type runtimeDispatch struct {
	kind   runtimeDispatchKind
	driver external.Driver
}

// resolveRuntimeDispatch decides which runtime executes a request's turn:
// the in-process model runtime, the ACP pool, or a direct external driver.
func (s *Service) resolveRuntimeDispatch(ctx context.Context, req ChatRequest) (runtimeDispatch, error) {
	if s == nil || s.sessionService == nil || strings.TrimSpace(req.ThreadID) == "" {
		return runtimeDispatch{kind: dispatchNative}, nil
	}
	sess, err := s.sessionService.Get(ctx, req.ThreadID)
	if err != nil {
		return runtimeDispatch{}, err
	}
	if err := validateSessionBot(req.BotID, req.ThreadID, sess.BotID); err != nil {
		return runtimeDispatch{}, err
	}
	runtimeType := ""
	switch {
	case session.IsACPRuntime(sess):
		runtimeType = session.RuntimeACPAgent
	case session.IsDirectRuntime(sess):
		runtimeType = strings.TrimSpace(sess.RuntimeType)
	default:
		return runtimeDispatch{kind: dispatchNative}, nil
	}
	driver, ok := s.externalDrivers[runtimeType]
	if !ok {
		return runtimeDispatch{}, apperror.New(apperror.CodeExternalRuntimeUnavailable, map[string]string{"runtime": runtimeType})
	}
	return runtimeDispatch{kind: dispatchExternal, driver: driver}, nil
}

// runtimeSessionMeta overlays session metadata with runtime metadata
// (runtime wins), giving drivers one merged view of their keys.
func runtimeSessionMeta(sess session.Thread) map[string]any {
	out := make(map[string]any, len(sess.Metadata)+len(sess.RuntimeMetadata))
	for key, value := range sess.Metadata {
		out[key] = value
	}
	for key, value := range sess.RuntimeMetadata {
		out[key] = value
	}
	return out
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
func (s *Service) AbortSessionRuns(ctx context.Context, botID, sessionID string) error {
	if s == nil || s.decisionRuntime == nil {
		return nil
	}
	snapshot, err := s.decisionRuntime.Snapshot(ctx, botID, sessionID)
	if err != nil {
		return err
	}
	run := snapshot.CurrentRunView
	if run == nil || strings.TrimSpace(run.RunID) == "" {
		return nil
	}
	runID := run.RunID
	if _, err = s.decisionRuntime.Abort(ctx, botID, sessionID, runID); err != nil {
		return err
	}
	deadline := time.Now().Add(abortSettleWait)
	for {
		snapshot, err := s.decisionRuntime.Snapshot(ctx, botID, sessionID)
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
			!s.hasExternalAgentActivePrompt(botID, sessionID) {
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
func (s *Service) handleRuntimeRoundRollback(ctx context.Context, driver external.Driver, req ChatRequest, runtimeType string, turnCompleted bool) {
	if turnCompleted {
		if handler, ok := driver.(external.RoundRollbackHandler); ok {
			handler.OnRoundRolledBack(context.WithoutCancel(ctx), req.BotID, req.ThreadID)
			return
		}
	}
	s.logRuntimeRoundDivergence(req, runtimeType)
}

func (s *Service) logRuntimeRoundDivergence(req ChatRequest, runtimeType string) {
	s.logger.Warn("external runtime remembers a round the visible history lost",
		slog.String("session_id", req.ThreadID),
		slog.String("run_id", req.RunID),
		slog.String("runtime", runtimeType),
	)
}

// runtimeTurnError marks err as the failure of a turn runtimeType ran. Every
// External Agent failure carries its runtime under the same key, so one query
// over the result records finds them whichever agent failed and whatever code
// the failure took.
func runtimeTurnError(runtimeType string, err error) error {
	return errs.WrapWithDepth(1, err, "external agent turn", slog.String("runtime", runtimeType))
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
