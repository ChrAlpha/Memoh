package transcript

import (
	"context"
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
)

// subagentThreadService is the slice of the thread service the direct-chat
// path needs beyond the narrow SessionService interface. *thread.Service
// satisfies it; test doubles that don't simply keep the plain-chat behavior.
type subagentThreadService interface {
	GetSubagentConfig(ctx context.Context, sessionID string) (sessionpkg.SubagentConfig, error)
	ListSubagentForkContext(ctx context.Context, sessionID string) ([]sessionpkg.SubagentForkContextMessage, error)
}

// ApplySubagentDefaults recognizes a direct turn on a subagent thread —
// a user chatting with a spawned agent from its own session view — and gives
// it the same execution surface a parent-driven task gets: the subagent
// system prompt, the pinned model, and no memory extraction or title rewrite.
// Turns that already carry a session type (schedules, the spawn
// path itself) and turns on non-subagent threads pass through untouched.
//
// hasMemory reports whether the session already persists a (model, effort)
// pair (issue #879): the pin is only the session's INITIAL pair and the user
// may override it, so once a remembered pair exists the pin must not refill
// the request over it (the resolution chain reads the memory level itself).
func (t *Reader) ApplySubagentDefaults(ctx context.Context, req runreq.ChatRequest, hasMemory bool) runreq.ChatRequest {
	if strings.TrimSpace(req.SessionType) != "" || strings.TrimSpace(req.ThreadID) == "" {
		return req
	}
	if t.sessions == nil {
		return req
	}
	sess, err := t.sessions.Get(ctx, req.ThreadID)
	if err != nil || !strings.EqualFold(strings.TrimSpace(sess.Type), sessionpkg.TypeSubagent) {
		return req
	}
	req.SessionType = sessionpkg.TypeSubagent
	// Subagent transcripts are task working state: they must not seed the
	// bot's long-term memory, and the session already carries its task title.
	req.SkipMemoryExtraction = true
	req.SkipTitleGeneration = true
	if !hasMemory && strings.TrimSpace(req.Model) == "" && strings.TrimSpace(req.Provider) == "" {
		if svc, ok := t.sessions.(subagentThreadService); ok {
			if config, cfgErr := svc.GetSubagentConfig(ctx, req.ThreadID); cfgErr == nil {
				switch {
				case strings.TrimSpace(config.ModelUUID) != "":
					req.Model = config.ModelUUID
				case strings.TrimSpace(config.ModelID) != "":
					req.Model = config.ModelID
					req.Provider = config.ProviderName
				}
			}
		}
	}
	return req
}

// SubagentForkMessages loads the parent-context snapshot a forked
// subagent inherited, in the shape resolve's message assembly consumes. The
// spawn path prepends the same rows on every parent-driven task; a direct
// turn has to do it too or the forked agent loses its inherited context the
// moment a human talks to it.
func (t *Reader) SubagentForkMessages(ctx context.Context, req runreq.ChatRequest) []runreq.ModelMessage {
	if !strings.EqualFold(strings.TrimSpace(req.SessionType), sessionpkg.TypeSubagent) {
		return nil
	}
	svc, ok := t.sessions.(subagentThreadService)
	if !ok {
		return nil
	}
	rows, err := svc.ListSubagentForkContext(ctx, req.ThreadID)
	if err != nil || len(rows) == 0 {
		return nil
	}
	messages := make([]sdk.Message, 0, len(rows))
	for _, row := range rows {
		// Fork rows are written in the stored shape (history.MarshalStoredSDKMessage).
		msg := historyfrag.StoredModelMessageToSDKMessage(historyfrag.DecodeStoredModelMessage(nil, "", row.Role, row.Message))
		if msg.Role == "" {
			msg.Role = sdk.MessageRole(row.Role)
		}
		if len(msg.Content) == 0 {
			continue
		}
		messages = append(messages, msg)
	}
	return runreq.SDKMessagesToModelMessages(messages)
}
