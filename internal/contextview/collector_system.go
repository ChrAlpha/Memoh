package contextview

import (
	"context"
	"errors"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
)

const systemPromptCollectorName = "system_prompt"

type SystemPromptConfig struct {
	System    string
	ToolUsage string
}

type SystemPromptCollector struct{}

func (*SystemPromptCollector) Name() string {
	return systemPromptCollectorName
}

func (*SystemPromptCollector) Collect(_ context.Context, req CollectRequest) ([]contextfrag.ContextFrag, error) {
	cfg, err := systemPromptConfig(req.Config)
	if err != nil {
		return nil, err
	}

	system := strings.TrimSpace(cfg.System)
	if system == "" {
		return nil, nil
	}

	toolUsage := strings.TrimSpace(cfg.ToolUsage)
	toolStart := -1
	if toolUsage != "" {
		toolStart = strings.Index(system, toolUsage)
	}
	if toolStart < 0 {
		return []contextfrag.ContextFrag{
			systemPromptTextFrag(req.Scope, "system.prompt", contextfrag.KindSystemPrompt, system, 20, contextfrag.SourceRunConfig, 0),
		}, nil
	}

	frags := make([]contextfrag.ContextFrag, 0, 3)
	if prefix := strings.TrimSpace(system[:toolStart]); prefix != "" {
		frags = append(frags, systemPromptTextFrag(req.Scope, "system.prompt", contextfrag.KindSystemPrompt, prefix, 20, contextfrag.SourceRunConfig, 0))
	}

	rest := strings.TrimSpace(system[toolStart:])
	toolEnd := len(toolUsage)
	if toolUsageText := strings.TrimSpace(rest[:toolEnd]); toolUsageText != "" {
		frags = append(frags, systemPromptTextFrag(req.Scope, "system.tool_usage", contextfrag.KindToolUsage, toolUsageText, 45, contextfrag.SourceAgentToolUsage, 1))
	}
	if suffix := strings.TrimSpace(rest[toolEnd:]); suffix != "" {
		kind := contextfrag.KindSystemPrompt
		id := "system.prompt.tail"
		if strings.HasPrefix(suffix, "## Workspace instruction files") {
			kind = contextfrag.KindWorkspaceInstruction
			id = "system.workspace_instructions"
		}
		frags = append(frags, systemPromptTextFrag(req.Scope, id, kind, suffix, 50, contextfrag.SourceRunConfig, 2))
	}
	return frags, nil
}

func systemPromptConfig(config any) (SystemPromptConfig, error) {
	if config == nil {
		return SystemPromptConfig{}, nil
	}
	switch cfg := config.(type) {
	case SystemPromptConfig:
		return cfg, nil
	case *SystemPromptConfig:
		if cfg == nil {
			return SystemPromptConfig{}, nil
		}
		return *cfg, nil
	default:
		return SystemPromptConfig{}, errors.New("system_prompt config must be SystemPromptConfig")
	}
}

func systemPromptTextFrag(scope contextfrag.Scope, id string, kind contextfrag.Kind, text string, priority int, source string, index int) contextfrag.ContextFrag {
	return contextfrag.TextFrag(contextfrag.TextFragInput{
		ID:         id,
		Kind:       kind,
		Role:       sdk.MessageRoleSystem,
		Slot:       contextfrag.SlotSystem,
		Text:       text,
		Priority:   priority,
		CacheClass: contextfrag.CacheStable,
		Trust:      contextfrag.TrustSystem,
		Scope:      scope,
		Source:     source,
		Collector:  systemPromptCollectorName,
		Index:      index,
		Render:     contextfrag.RenderPolicy{Format: contextfrag.RenderMarkdown},
	})
}
