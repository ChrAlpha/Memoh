package application

import (
	"context"
	"strings"

	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
)

// subagentThreadService is the slice of the thread service the direct-chat
// path needs beyond the narrow SessionService interface. *thread.Service
// satisfies it; test doubles that don't simply keep the plain-chat behavior.
type subagentThreadService interface {
	GetSubagentConfig(ctx context.Context, sessionID string) (sessionpkg.SubagentConfig, error)
	ListSubagentForkContext(ctx context.Context, sessionID string) ([]sessionpkg.SubagentForkContextMessage, error)
}

// applySubagentThreadDefaults recognizes a direct turn on a subagent thread —
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
func (s *Service) applySubagentThreadDefaults(ctx context.Context, req ChatRequest, hasMemory bool) ChatRequest {
	if strings.TrimSpace(req.SessionType) != "" || strings.TrimSpace(req.ThreadID) == "" {
		return req
	}
	if s == nil || s.sessionService == nil {
		return req
	}
	sess, err := s.sessionService.Get(ctx, req.ThreadID)
	if err != nil || !strings.EqualFold(strings.TrimSpace(sess.Type), sessionpkg.TypeSubagent) {
		return req
	}
	req.SessionType = sessionpkg.TypeSubagent
	// Subagent transcripts are task working state: they must not seed the
	// bot's long-term memory, and the session already carries its task title.
	req.SkipMemoryExtraction = true
	req.SkipTitleGeneration = true
	if !hasMemory && strings.TrimSpace(req.Model) == "" && strings.TrimSpace(req.Provider) == "" {
		if svc, ok := s.sessionService.(subagentThreadService); ok {
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
