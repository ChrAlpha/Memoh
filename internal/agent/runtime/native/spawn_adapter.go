package native

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
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
		return spawnFailureResult(rc), err
	}

	spawnResult := &tools.SpawnResult{
		Messages: result.Messages,
		Text:     result.Text,
		Usage:    result.Usage,
	}
	if snapshot, ok := rc.ContextLifecycle.Snapshot(); ok {
		spawnResult.ContextLifecycle = &snapshot
	}
	return spawnResult, nil
}

func runConfigFromSpawnRunConfig(cfg tools.SpawnRunConfig) RunConfig {
	runID := strings.TrimSpace(cfg.RunID)
	if runID == "" {
		runID = uuid.NewString()
	}
	messages := cfg.Messages
	var currentUserMessageIndex *int
	if cfg.Query != "" {
		now := time.Now().UTC()
		if cfg.Identity.TimezoneLocation != nil {
			now = now.In(cfg.Identity.TimezoneLocation)
		}
		messages = append(messages, sdk.Message{
			Role:    sdk.MessageRoleUser,
			Content: []sdk.MessagePart{sdk.TextPart{Text: "Current time: " + now.Format(time.RFC3339) + "\n" + cfg.Query}},
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
		RunID:                          runID,
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
		ContextLifecycle: contextfrag.NewLifecycleHolder(),
	}
	rc.ContextSourceFrags = SpawnContextSourceFrags(rc)
	return rc
}

// SpawnContextSourceFrags builds the fragments-first ContextSourceFrags for a
// subagent run: typed system sections (the same minimal params
// SpawnSystemPrompt renders from) plus history fragments compiled from the
// already-materialized rc.Messages/rc.Query. internal/contextview can't be
// imported here — it imports internal/agent for RunConfig, so the reverse
// import would cycle — so this reuses contextfrag.CompileFrags directly
// instead of contextview's collectors, then reproduces the two behaviors
// contextview.HistoryMessagesCollector layers on top: pinning every history
// fragment against budget trimming (subagents never set
// ContextTrimmableMessages, so today every history message is implicitly
// must-keep) and repairing dangling tool-call closures (subagent history is a
// raw, unsanitized session load and can legitimately end mid tool-call).
// Exported so internal/contextview's cross-package equivalence test can
// exercise it directly (see spawn_frags_first_test.go).
func SpawnContextSourceFrags(rc RunConfig) []contextfrag.ContextFrag {
	sections := GenerateSystemSections(SystemPromptParams{SessionType: rc.SessionType})
	sectionFrags := SystemSectionFrags(sections, rc.ContextScope)

	query := rc.Query
	if rc.ContextQueryMaterialized {
		// The query is already the trailing message inside rc.Messages (see
		// above), so it must not also compile into a separate current-user
		// fragment.
		query = ""
	}
	frags := contextfrag.CompileFrags(contextfrag.CompileInput{
		Scope:                   rc.ContextScope,
		Messages:                rc.Messages,
		CurrentUserMessageIndex: rc.ContextCurrentUserMessageIndex,
		Query:                   query,
	})

	history := make([]contextfrag.ContextFrag, 0, len(frags))
	current := make([]contextfrag.ContextFrag, 0, 1)
	for _, frag := range frags {
		switch frag.Slot {
		case contextfrag.SlotHistory:
			history = append(history, frag)
		case contextfrag.SlotCurrentUser:
			current = append(current, frag)
		}
	}
	for i := range history {
		if i >= rc.ContextTrimmableMessages {
			history[i].Budget.Overflow = contextfrag.OverflowKeep
		}
	}
	history = contextfrag.RepairToolClosureFrags(history, rc.ContextScope, contextfrag.CollectorRunConfigFields)

	combined := make([]contextfrag.ContextFrag, 0, len(sectionFrags)+len(history)+len(current))
	combined = append(combined, sectionFrags...)
	combined = append(combined, history...)
	return append(combined, current...)
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
			return spawnFailureResult(rc), cause
		}
		return spawnFailureResult(rc), ctx.Err()
	}
	if streamErr != nil {
		return spawnFailureResult(rc), streamErr
	}

	spawnResult := &tools.SpawnResult{
		Messages: finalMessages,
		Text:     allText.String(),
		Usage:    &totalUsage,
	}
	if snapshot, ok := rc.ContextLifecycle.Snapshot(); ok {
		spawnResult.ContextLifecycle = &snapshot
	}
	return spawnResult, nil
}

func spawnFailureResult(rc RunConfig) *tools.SpawnResult {
	if rc.ContextLifecycle == nil {
		return nil
	}
	snapshot, ok := rc.ContextLifecycle.Snapshot()
	if !ok {
		return nil
	}
	return &tools.SpawnResult{ContextLifecycle: &snapshot}
}

// SpawnSystemPrompt returns the system prompt for a given session type.
func SpawnSystemPrompt(sessionType string) string {
	return GenerateSystemPrompt(SystemPromptParams{
		SessionType: sessionType,
	})
}
