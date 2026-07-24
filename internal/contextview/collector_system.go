package contextview

import (
	"context"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
)

const systemPromptCollectorName = "system_prompt"

type SystemPromptConfig struct {
	System    string
	ToolUsage string
	// SplitWorkspace splits the workspace instruction section into its own
	// fragment even without embedded tool usage, so an agent-side tool usage
	// fragment can sort between prompt and workspace instructions.
	SplitWorkspace bool
}

// SystemPromptCollector reverse-parses a flat system prompt string into
// system-slot fragments. Production paths (chat/heartbeat/schedule, subagent,
// discuss) now build typed fragments forward via agentpkg.SystemSectionFrags;
// this collector remains only as a fallback: the legacy no-source-frags
// branch of ApplyProviderRunConfig and discuss inputs without SystemFrags.
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
		if cfg.SplitWorkspace {
			if idx := strings.Index(system, contextfrag.WorkspaceInstructionAnchor); idx >= 0 {
				frags := make([]contextfrag.ContextFrag, 0, 2)
				if prompt := strings.TrimSpace(system[:idx]); prompt != "" {
					frags = append(frags, systemPromptTextFrag(req.Scope, "system.prompt", contextfrag.KindSystemPrompt, prompt, 20, contextfrag.SourceRunConfig, 0))
				}
				frags = append(frags, systemPromptTextFrag(req.Scope, "system.workspace_instructions", contextfrag.KindWorkspaceInstruction, strings.TrimSpace(system[idx:]), 50, contextfrag.SourceRunConfig, 1))
				return frags, nil
			}
		}
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
	return collectorConfig[SystemPromptConfig](config, "system_prompt config must be SystemPromptConfig")
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
