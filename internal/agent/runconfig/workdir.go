package runconfig

import (
	"context"
	"errors"
	"strings"

	"github.com/felinics/memoh/internal/agent/agentapi/ports"
	"github.com/felinics/memoh/internal/workdir"
)

// ErrWorkspaceTargetWorkdirConflict is returned when a request tries to move
// a workdir-bound session onto a different workspace target. The binding is
// immutable: the workdir's directory only exists on the workdir's target, so
// honoring the switch would produce a session whose working directory does
// not exist.
var ErrWorkspaceTargetWorkdirConflict = errors.New(
	"this session is bound to a workdir; its workspace target cannot be changed")

// workdirBindings answers which workdir a session is bound to.
type workdirBindings interface {
	Binding(ctx context.Context, botID, sessionID string) (workdir.Resolved, bool, error)
}

// sessionWorkdirs loads the session's workdir binding, if any. Errors fail the
// turn rather than degrade: silently dropping the binding would run the turn in
// the wrong directory, the exact bug class workdir bindings exist to eliminate.
type sessionWorkdirs struct {
	sessions ports.SessionService
	workdirs ports.WorkdirResolver
}

func (w sessionWorkdirs) Binding(ctx context.Context, botID, sessionID string) (workdir.Resolved, bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return workdir.Resolved{}, false, nil
	}
	sess, err := w.sessions.Get(ctx, sessionID)
	if err != nil {
		return workdir.Resolved{}, false, err
	}
	if strings.TrimSpace(sess.WorkdirID) == "" {
		return workdir.Resolved{}, false, nil
	}
	resolved, err := w.workdirs.ResolveForSession(ctx, botID, sess.WorkdirID)
	if err != nil {
		return workdir.Resolved{}, false, err
	}
	return resolved, true, nil
}
