package botenv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/hooks"
	"github.com/felinics/memoh/internal/workspace/bridge"
)

func (e *Env) ApplyUserMessageHook(ctx context.Context, req runreq.ChatRequest) (runreq.ChatRequest, error) {
	res, err := e.RunChatHook(ctx, req, hooks.EventUserMessageReceived, func(hreq *hooks.Request) {
		hreq.Channel = map[string]any{
			"platform":          strings.TrimSpace(req.CurrentChannel),
			"conversation_type": strings.TrimSpace(req.ConversationType),
			"reply_target":      strings.TrimSpace(req.ReplyTarget),
		}
		hreq.Extra = map[string]any{
			"query":               strings.TrimSpace(req.Query),
			"raw_query":           strings.TrimSpace(req.RawQuery),
			"attachment_count":    len(req.Attachments),
			"external_message_id": strings.TrimSpace(req.ExternalMessageID),
		}
	})
	if err != nil {
		if errors.Is(err, hooks.ErrDenied) || res.Decision == hooks.DecisionDeny {
			return req, fmt.Errorf("%w: %s", hooks.ErrDenied, firstHookText(res.Reason, err.Error()))
		}
		return req, err
	}
	if strings.TrimSpace(res.AppendContext) != "" {
		req.Messages = append(req.Messages, runreq.ModelMessage{
			Role:    "user",
			Content: runreq.NewTextContent(FormatHookContext(hooks.EventUserMessageReceived, res.AppendContext)),
		})
	}
	// The reserved-skill-metadata guard runs at the service entrypoints
	// BEFORE this hook, so hook-modified messages must be re-checked here.
	// Today the only mutation is the metadata-less text append above, but the
	// guard is a security boundary (reserved keys are trusted downstream as
	// server-authored state) and must hold for any future hook capability
	// that can shape message content or metadata.
	if err := runreq.RejectReservedSkillMetadataIfPresent(req); err != nil {
		return req, err
	}
	return req, nil
}

func (e *Env) RunPromptHook(ctx context.Context, cfg PromptView, eventName string) hooks.Result {
	res, err := e.runBaseHook(ctx, cfg.BotID, cfg.SessionID, cfg.ChatID, eventName, func(req *hooks.Request) {
		req.Turn = map[string]any{
			"session_type":  cfg.SessionType,
			"message_count": cfg.MessageCount,
			"system_bytes":  cfg.SystemBytes,
		}
	})
	if err != nil {
		e.LogHookWarn(eventName, cfg.BotID, cfg.SessionID, err)
		return hooks.Result{}
	}
	res.AppendContext = strings.TrimSpace(res.AppendContext)
	return res
}

type PromptView struct {
	BotID        string
	SessionID    string
	ChatID       string
	SessionType  string
	MessageCount int
	SystemBytes  int
}

func (e *Env) RunChatHook(ctx context.Context, chatReq runreq.ChatRequest, eventName string, enrich func(*hooks.Request)) (hooks.Result, error) {
	return e.runBaseHook(ctx, chatReq.BotID, chatReq.ThreadID, chatReq.ChatID, eventName, enrich)
}

func (e *Env) runBaseHook(ctx context.Context, botID, sessionID, chatID, eventName string, enrich func(*hooks.Request)) (hooks.Result, error) {
	return e.hooks.run(ctx, e, botID, sessionID, chatID, eventName, enrich)
}

// hookRunner dispatches one hook event. Without a hook service every event is
// allowed and nothing is built or looked up.
type hookRunner interface {
	run(ctx context.Context, e *Env, botID, sessionID, chatID, eventName string, enrich func(*hooks.Request)) (hooks.Result, error)
}

type noHooks struct{}

func (noHooks) run(_ context.Context, _ *Env, _, _, _, eventName string, _ func(*hooks.Request)) (hooks.Result, error) {
	return hooks.Result{Decision: hooks.DecisionAllow, RuntimeSupported: hooks.RuntimeSupported(eventName)}, nil
}

type serviceHooks struct{ service *hooks.Service }

func (h serviceHooks) run(ctx context.Context, e *Env, botID, sessionID, chatID, eventName string, enrich func(*hooks.Request)) (hooks.Result, error) {
	req := hooks.Request{
		Version:   1,
		Event:     eventName,
		BotID:     strings.TrimSpace(botID),
		SessionID: strings.TrimSpace(sessionID),
		ChatID:    strings.TrimSpace(chatID),
		Workspace: e.hookWorkspace(ctx, botID),
	}
	if enrich != nil {
		enrich(&req)
	}
	return h.service.Run(ctx, req, nil)
}

func (e *Env) hookWorkspace(ctx context.Context, botID string) hooks.WorkspaceInfo {
	info := hooks.WorkspaceInfo{
		CWD:     hooks.DefaultWorkDir,
		Runtime: bridge.WorkspaceBackendContainer,
	}
	provider, ok := e.bridge.(bridge.WorkspaceInfoProvider)
	if !ok {
		return info
	}
	workspaceInfo, err := provider.WorkspaceInfo(ctx, botID)
	if err != nil {
		return info
	}
	if strings.TrimSpace(workspaceInfo.DefaultWorkDir) != "" {
		info.CWD = workspaceInfo.DefaultWorkDir
	}
	if strings.TrimSpace(workspaceInfo.Backend) != "" {
		info.Runtime = workspaceInfo.Backend
	}
	return info
}

func (e *Env) LogHookWarn(eventName, botID, sessionID string, err error) {
	if err == nil {
		return
	}
	e.logger.Warn("hook failed",
		slog.String("event", eventName),
		slog.String("bot_id", botID),
		slog.String("session_id", sessionID),
		slog.Any("error", err),
	)
}

func FormatHookContext(eventName, text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return "[Hook Context: " + strings.TrimSpace(eventName) + "]\n" + text
}

// AfterPromptSystemBytes preserves the historical SystemBytes observation
// while hook system sections travel separately as typed fragments.
func AfterPromptSystemBytes(system string, hookTexts []string) int {
	n := len(system)
	if len(hookTexts) > 0 {
		n += len("\n\n") + len(strings.Join(hookTexts, "\n\n"))
	}
	return n
}

func firstHookText(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
