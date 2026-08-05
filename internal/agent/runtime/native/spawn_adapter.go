package native

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	tools "github.com/memohai/memoh/internal/agent/tool"
)

// SpawnAdapter wraps *Agent to satisfy tools.SpawnAgent without creating
// an import cycle (tools -> agent).
type SpawnAdapter struct {
	agent *Agent
}

// NewSpawnAdapter creates a SpawnAdapter from the given Agent.
func NewSpawnAdapter(a *Agent) *SpawnAdapter {
	return &SpawnAdapter{agent: a}
}

func (s *SpawnAdapter) Generate(ctx context.Context, cfg tools.SpawnRunConfig) (*tools.SpawnResult, error) {
	rc := runConfigFromSpawnRunConfig(cfg)

	result, err := s.agent.Generate(ctx, rc)
	if err != nil {
		return nil, err
	}

	return &tools.SpawnResult{
		Messages: result.Messages,
		Text:     result.Text,
		Usage:    result.Usage,
	}, nil
}

func runConfigFromSpawnRunConfig(cfg tools.SpawnRunConfig) RunConfig {
	messages := cfg.Messages
	var currentUserMessageIndex *int
	if cfg.Query != "" {
		messages = append(messages, sdk.Message{
			Role:    sdk.MessageRoleUser,
			Content: []sdk.MessagePart{sdk.TextPart{Text: cfg.Query}},
		})
		index := len(messages) - 1
		currentUserMessageIndex = &index
	}

	identity := SessionContext{
		BotID:               cfg.Identity.BotID,
		ChatID:              cfg.Identity.ChatID,
		SessionID:           cfg.Identity.SessionID,
		UserID:              cfg.Identity.UserID,
		ChannelIdentityID:   cfg.Identity.ChannelIdentityID,
		CurrentPlatform:     cfg.Identity.CurrentPlatform,
		ReplyTarget:         cfg.Identity.ReplyTarget,
		ConversationType:    cfg.Identity.ConversationType,
		SessionToken:        cfg.Identity.SessionToken,
		WorkspaceTargetID:   cfg.Identity.WorkspaceTargetID,
		WorkspaceTargetKind: cfg.Identity.WorkspaceTargetKind,
		WorkspaceTargetName: cfg.Identity.WorkspaceTargetName,
		TimezoneLocation:    cfg.Identity.TimezoneLocation,
		IsSubagent:          cfg.Identity.IsSubagent,
	}
	skills := make([]SkillEntry, 0, len(cfg.Skills))
	for name, skill := range cfg.Skills {
		skills = append(skills, SkillEntry{
			Name:        name,
			Description: skill.Description,
			Content:     skill.Content,
			Path:        skill.Path,
		})
	}
	rc := RunConfig{
		Model:                          cfg.Model,
		CurrentModelUUID:               cfg.ModelUUID,
		CurrentModelID:                 cfg.ModelID,
		CurrentModelProvider:           cfg.ModelProvider,
		System:                         cfg.System,
		Query:                          cfg.Query,
		ContextQueryMaterialized:       cfg.Query != "",
		ContextCurrentUserMessageIndex: currentUserMessageIndex,
		SessionType:                    cfg.SessionType,
		Messages:                       messages,
		ReasoningEffort:                cfg.ReasoningEffort,
		PromptCacheTTL:                 cfg.PromptCacheTTL,
		ChatCompletionsCompat:          cfg.ChatCompletionsCompat,
		SupportsImageInput:             cfg.SupportsImageInput,
		SupportsToolCall:               cfg.SupportsToolCall,
		Identity:                       identity,
		Skills:                         skills,
		BackgroundManager:              cfg.BackgroundManager,
		ContextBudgetMaxTokens:         cfg.ContextBudgetMaxTokens,
		ContextToolExchangePolicy:      cfg.ContextToolExchangePolicy,
		ContextScope: contextfrag.Scope{
			BotID:             identity.BotID,
			ChatID:            identity.ChatID,
			SessionID:         identity.SessionID,
			ChannelIdentityID: identity.ChannelIdentityID,
			Platform:          identity.CurrentPlatform,
		},
		LoopDetection: LoopDetectionConfig{
			Enabled: cfg.LoopDetection.Enabled,
		},
	}
	rc.ContextSourceFrags = SpawnContextSourceFrags(rc)
	return rc
}

// SpawnContextSourceFrags uses typed system sections only when they reproduce
// the caller-supplied System exactly. Custom spawn prompts deliberately stay
// on the legacy collector fallback so PR1 cannot replace or normalize them.
func SpawnContextSourceFrags(rc RunConfig) []contextfrag.ContextFrag {
	sections := GenerateSystemSections(SystemPromptParams{SessionType: rc.SessionType})
	if renderSystemSections(sections) != rc.System {
		return nil
	}
	query := rc.Query
	if rc.ContextQueryMaterialized {
		query = ""
	}
	messages := contextfrag.CompileFrags(contextfrag.CompileInput{
		Scope:                   rc.ContextScope,
		Messages:                rc.Messages,
		CurrentUserMessageIndex: rc.ContextCurrentUserMessageIndex,
		Query:                   query,
	})
	frags := SystemSectionFrags(sections, rc.ContextScope)
	return append(frags, messages...)
}

// GenerateWithWatchdog runs the agent in streaming mode, touching the
// provided touchFn on every stream event (token, tool progress, etc.).
// It collects the full result and returns it in the same shape as Generate.
// This enables activity-based watchdog monitoring for subagent execution.
func (s *SpawnAdapter) GenerateWithWatchdog(ctx context.Context, cfg tools.SpawnRunConfig, touchFn func()) (*tools.SpawnResult, error) {
	rc := runConfigFromSpawnRunConfig(cfg)

	// Use Stream instead of Generate to get per-token/per-tool activity signals.
	eventCh := s.agent.Stream(ctx, rc)

	var allText strings.Builder
	var finalMessages []sdk.Message
	var totalUsage sdk.Usage
	var streamErr error

	for evt := range eventCh {
		// Touch the watchdog on every event — this is the activity signal.
		touchFn()

		switch evt.Type {
		case EventTextDelta:
			allText.WriteString(evt.Delta)
		case EventError:
			if streamErr == nil {
				streamErr = errors.New(evt.Error)
			}
		case EventAgentEnd, EventAgentAbort:
			if evt.Messages != nil {
				_ = json.Unmarshal(evt.Messages, &finalMessages)
			}
			if evt.Usage != nil {
				_ = json.Unmarshal(evt.Usage, &totalUsage)
			}
		}
	}

	// Check if context was cancelled (watchdog fired or parent cancelled).
	if ctx.Err() != nil {
		if cause := context.Cause(ctx); cause != nil {
			return nil, cause
		}
		return nil, ctx.Err()
	}
	if streamErr != nil {
		return nil, streamErr
	}

	return &tools.SpawnResult{
		Messages: finalMessages,
		Text:     allText.String(),
		Usage:    &totalUsage,
	}, nil
}

// SpawnSystemPrompt returns the system prompt for a given session type.
func SpawnSystemPrompt(sessionType string) string {
	return GenerateSystemPrompt(SystemPromptParams{
		SessionType: sessionType,
	})
}
