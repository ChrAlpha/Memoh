package runconfig

import (
	"context"
	"log/slog"
	"strings"

	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/sessionfacts"
)

// requestRunFacts returns the facts a chat request runs under. A request that
// names no mode is a message answered on the streaming path: its reply reaches
// the sender, who can also answer decisions.
func (b *Builder) requestRunFacts(ctx context.Context, req runreq.ChatRequest) sessionfacts.Facts {
	if strings.TrimSpace(req.SessionType) == "" {
		return sessionfacts.Preset(sessionmode.Chat)
	}
	_, _, stored := b.sessionDescriptor(ctx, req.ThreadID)
	return runFacts(stored, req.SessionType)
}

// runFacts returns the facts one run executes under: the session's own,
// except that a schedule fire has nobody to answer a decision even in a
// session people converse in.
func runFacts(session sessionfacts.Facts, runMode string) sessionfacts.Facts {
	if strings.TrimSpace(runMode) == sessionmode.Schedule {
		session.DecisionMode = sessionfacts.DecisionAutoReject
	}
	return session
}

func (b *Builder) sessionDescriptor(ctx context.Context, sessionID string) (string, string, sessionfacts.Facts) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return sessionpkg.TypeChat, sessionpkg.RuntimeModel, sessionfacts.Preset(sessionpkg.TypeChat)
	}
	sess, err := b.sessions.Get(ctx, sessionID)
	if err != nil {
		b.logger.WarnContext(ctx, "ResolveRunConfig: session lookup failed; falling back to chat session type",
			slog.String("session_id", sessionID),
			slog.Any("error", err),
		)
		return sessionpkg.TypeChat, sessionpkg.RuntimeModel, sessionfacts.Preset(sessionpkg.TypeChat)
	}
	typ := strings.TrimSpace(sess.Type)
	if typ == "" {
		typ = sessionpkg.TypeChat
	}
	runtimeType := strings.TrimSpace(sess.RuntimeType)
	if runtimeType == "" {
		if sessionpkg.IsACPRuntime(sess) {
			runtimeType = sessionpkg.RuntimeACPAgent
		} else {
			runtimeType = sessionpkg.RuntimeModel
		}
	}
	return typ, runtimeType, sess.Facts
}
