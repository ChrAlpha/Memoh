package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/agent/tools"
	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/models"
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
	if cfg.Query != "" {
		now := time.Now().UTC()
		if cfg.Identity.TimezoneLocation != nil {
			now = now.In(cfg.Identity.TimezoneLocation)
		}
		messages = append(messages, sdk.Message{
			Role:    sdk.MessageRoleUser,
			Content: []sdk.MessagePart{sdk.TextPart{Text: "Current time: " + now.Format(time.RFC3339) + "\n" + cfg.Query}},
		})
	}

	identity := SessionContext{
		BotID:             cfg.Identity.BotID,
		ChatID:            cfg.Identity.ChatID,
		SessionID:         cfg.Identity.SessionID,
		ChannelIdentityID: cfg.Identity.ChannelIdentityID,
		CurrentPlatform:   cfg.Identity.CurrentPlatform,
		SessionToken:      cfg.Identity.SessionToken,
		IsSubagent:        cfg.Identity.IsSubagent,
		TimezoneLocation:  cfg.Identity.TimezoneLocation,
	}
	rc := RunConfig{
		Model:                     cfg.Model,
		System:                    cfg.System,
		Query:                     cfg.Query,
		ContextQueryMaterialized:  cfg.Query != "",
		SessionType:               cfg.SessionType,
		Messages:                  messages,
		ReasoningEffort:           cfg.ReasoningEffort,
		PromptCacheTTL:            cfg.PromptCacheTTL,
		SupportsToolCall:          true,
		Identity:                  identity,
		ContextBudgetMaxTokens:    cfg.ContextBudgetMaxTokens,
		ContextToolExchangePolicy: cfg.ContextToolExchangePolicy,
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
		Scope:    rc.ContextScope,
		Messages: rc.Messages,
		Query:    query,
	})

	history := make([]contextfrag.ContextFrag, 0, len(frags))
	for _, frag := range frags {
		if frag.Slot == contextfrag.SlotHistory {
			history = append(history, frag)
		}
	}
	for i := range history {
		if i >= rc.ContextTrimmableMessages {
			history[i].Budget.Overflow = contextfrag.OverflowKeep
		}
	}
	history = contextfrag.RepairToolClosureFrags(history, rc.ContextScope, contextfrag.CollectorRunConfigFields)

	return append(sectionFrags, history...)
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

	for evt := range eventCh {
		// Touch the watchdog on every event — this is the activity signal.
		touchFn()

		switch evt.Type {
		case EventTextDelta:
			allText.WriteString(evt.Delta)
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

// SpawnModelCreatorFunc returns a tools.ModelCreator backed by the shared SDK model factory.
// This keeps subagent model creation aligned with the shared SDK model factory.
func SpawnModelCreatorFunc() tools.ModelCreator {
	return func(modelID, clientType, apiKey, codexAccountID, baseURL string, httpClient *http.Client) *sdk.Model {
		return models.NewSDKChatModel(models.SDKModelConfig{
			ModelID:        modelID,
			ClientType:     clientType,
			APIKey:         apiKey,
			CodexAccountID: codexAccountID,
			BaseURL:        baseURL,
			HTTPClient:     httpClient,
		})
	}
}
