package runconfig

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/decide"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/oauthctx"
	"github.com/felinics/memoh/internal/providers"
	"github.com/felinics/memoh/internal/reasoning"
	"github.com/felinics/memoh/internal/sessionfacts"
	"github.com/felinics/memoh/internal/settings"
)

// baseRunConfigParams holds parameters for buildBaseRunConfig that differ
// between chat and discuss callers.
type baseParams struct {
	BotID             string
	ChatID            string
	SessionID         string
	RouteID           string
	UserID            string
	ChannelIdentityID string
	CurrentPlatform   string
	ReplyTarget       string
	ConversationType  string
	SessionToken      string //nolint:gosec // session credential material, not a hardcoded secret
	SessionType       string
	// Facts are the session facts the run executes under (see runFacts).
	Facts sessionfacts.Facts
	// SessionPrefModelID/SessionPrefEffort carry the session's persisted
	// (model, effort) pair (issue #879), loaded by the caller before the
	// subagent pin may fill the request. Empty = no memory.
	SessionPrefModelID string
	SessionPrefEffort  string
	Model              string
	Provider           string
	ReasoningEffort    string // caller-provided override (empty = use bot default)
}

// buildBaseRunConfig creates a RunConfig with model, credentials, skills,
// identity and system prompt — everything except Messages/Query/InlineImages.
// Both resolve() and ResolveRunConfig() delegate to this shared builder.
func (b *Builder) buildBase(ctx context.Context, p baseParams) (native.RunConfig, models.GetResponse, sqlc.Provider, error) {
	botSettings, err := b.bot.LoadSettings(ctx, p.BotID)
	if err != nil {
		return native.RunConfig{}, models.GetResponse{}, sqlc.Provider{}, err
	}
	botInfo, loopDetectionEnabled := b.bot.LoadRuntimeInfo(ctx, p.BotID)
	userTimezoneName, userClockLocation := b.bot.ResolveTimezone(ctx, p.BotID, p.UserID)

	chatID := p.ChatID
	if chatID == "" {
		chatID = p.BotID
	}

	req := buildModelSelectionRequest(p, chatID)

	chatModel, provider, err := b.selectChatModel(ctx, req, botSettings, p.SessionPrefModelID)
	if err != nil {
		if errors.Is(err, errChatModelNotConfigured) {
			err = apperror.Wrap(apperror.CodeAgentChatModelNotConfigured, err, nil)
		}
		return native.RunConfig{}, models.GetResponse{}, sqlc.Provider{}, err
	}

	authCtx := oauthctx.WithUserID(ctx, p.UserID)
	creds, err := b.credentials.ResolveModelCredentials(authCtx, provider)
	if err != nil {
		return native.RunConfig{}, models.GetResponse{}, sqlc.Provider{}, fmt.Errorf("resolve provider credentials: %w", err)
	}

	baseURL := providers.ProviderConfigString(provider, "base_url")
	chatCompletionsCompat := models.ResolveChatCompletionsCompat(
		baseURL,
		providers.ProviderConfigString(provider, models.ChatCompletionsCompatConfigKey),
	)

	reasoningConfig := resolveRunReasoningConfig(chatModel, botSettings, p, provider.ClientType)

	sdkModel := models.NewSDKChatModel(models.SDKModelConfig{
		ModelID:               chatModel.ModelID,
		ClientType:            provider.ClientType,
		APIKey:                creds.APIKey,
		CodexAccountID:        creds.CodexAccountID,
		BaseURL:               baseURL,
		ChatCompletionsCompat: chatCompletionsCompat,
		HTTPClient:            b.httpClient,
		ReasoningConfig:       reasoningConfig,
		ReasoningDialect:      chatModel.Config.ReasoningDialect,
		ReasoningOffSupport:   chatModel.Config.ReasoningOffSupport,
		ReasoningDefaultOn:    chatModel.Config.ReasoningDefaultOn,
		ThinkingBudgetMin:     chatModel.Config.ThinkingBudgetMin,
		ThinkingBudgetMax:     chatModel.Config.ThinkingBudgetMax,
		ContextWindow:         contextBudgetFromChatModel(chatModel),
		MaxOutputTokens:       chatModel.Config.MaxOutputTokensLimit(),
	})

	var agentSkills []native.SkillEntry
	entries, skillErr := b.skills.LoadSkills(ctx, p.BotID)
	if skillErr != nil {
		b.logger.WarnContext(ctx, "failed to load skills", slog.String("bot_id", p.BotID), slog.Any("error", skillErr))
	} else {
		for _, e := range entries {
			if skill, ok := normalizeGatewaySkill(e); ok {
				agentSkills = append(agentSkills, skill)
			}
		}
	}
	if agentSkills == nil {
		agentSkills = []native.SkillEntry{}
	}

	cfg := native.RunConfig{
		Model:                    sdkModel,
		CurrentModelUUID:         chatModel.ID,
		CurrentModelID:           chatModel.ModelID,
		CurrentModelProvider:     provider.Name,
		CurrentModelProviderID:   chatModel.ProviderID,
		ReasoningConfig:          reasoningConfig,
		ReasoningStoredEffort:    strings.TrimSpace(botSettings.ReasoningEffort),
		ReasoningRequestedEffort: strings.TrimSpace(p.ReasoningEffort),
		ChatCompletionsCompat:    chatCompletionsCompat,
		PromptCacheTTL:           providers.ProviderConfigString(provider, "prompt_cache_ttl"),
		SessionType:              p.SessionType,
		ReplyDelivery:            p.Facts.ReplyDelivery,
		DecisionMode:             p.Facts.DecisionMode,
		SupportsImageInput:       supportsImageInputForModel(chatModel),
		SupportsFileInput:        supportsFileInputForModel(chatModel),
		SupportsToolCall:         chatModel.HasCompatibility(models.CompatToolCall),
		ModelMaxOutputTokens:     chatModel.Config.MaxOutputTokensLimit(),
		Identity: native.SessionContext{
			BotID:             p.BotID,
			ChatID:            chatID,
			SessionID:         p.SessionID,
			UserID:            strings.TrimSpace(p.UserID),
			ChannelIdentityID: strings.TrimSpace(p.ChannelIdentityID),
			CurrentPlatform:   p.CurrentPlatform,
			ReplyTarget:       strings.TrimSpace(p.ReplyTarget),
			ConversationType:  strings.TrimSpace(p.ConversationType),
			Timezone:          userTimezoneName,
			TimezoneLocation:  userClockLocation,
			SessionToken:      p.SessionToken,
		},
		Bot:               botInfo,
		Skills:            agentSkills,
		LoopDetection:     native.LoopDetectionConfig{Enabled: loopDetectionEnabled},
		BackgroundManager: b.background,
		ContextLifecycle:  contextfrag.NewLifecycleHolder(),
		ContextScope: contextfrag.Scope{
			BotID:             p.BotID,
			ChatID:            chatID,
			SessionID:         p.SessionID,
			ChannelIdentityID: strings.TrimSpace(p.ChannelIdentityID),
			Platform:          strings.TrimSpace(p.CurrentPlatform),
			ConversationType:  strings.TrimSpace(p.ConversationType),
			ReplyTarget:       strings.TrimSpace(p.ReplyTarget),
		},
	}
	cfg.ToolApprovalHandler = b.decider.ApprovalHandler(decide.HandlerParams{
		BotID:             p.BotID,
		SessionID:         p.SessionID,
		RouteID:           p.RouteID,
		ChannelIdentityID: p.ChannelIdentityID,
		CurrentPlatform:   p.CurrentPlatform,
		ReplyTarget:       p.ReplyTarget,
		ConversationType:  p.ConversationType,
		DecisionMode:      p.Facts.DecisionMode,
	})
	// Entries here carry no end user to authorize: the target was authorized
	// when the entry (or the session's workdir) chose it, so no principal.
	_, ws, wsErr := b.ResolveWorkspace(ctx, WorkspaceInput{
		BotID:      p.BotID,
		SessionID:  p.SessionID,
		BestEffort: true,
	})
	if wsErr != nil {
		return native.RunConfig{}, models.GetResponse{}, sqlc.Provider{}, wsErr
	}
	cfg.Identity.WorkdirPath = ws.WorkdirPath
	if ws.Target != nil {
		cfg.Identity.WorkspaceTargetID = ws.Target.TargetID
		cfg.Identity.WorkspaceTargetKind = ws.Target.Kind
		cfg.Identity.WorkspaceTargetName = ws.Target.Name
	}

	return cfg, chatModel, provider, nil
}

func supportsImageInputForModel(m models.GetResponse) bool {
	return m.HasCompatibility(models.CompatVision)
}

func supportsFileInputForModel(m models.GetResponse) bool {
	return m.HasCompatibility(models.CompatFileInput)
}

// resolveRunReasoningConfig keeps remembered effort attached to its model UUID.
// An explicit switch starts from the new model default; a slug resolving to
// the same UUID is not a switch and may retain the remembered effort.
func resolveRunReasoningConfig(chatModel models.GetResponse, botSettings settings.Settings, p baseParams, clientType string) *models.ReasoningConfig {
	sessionEffort := p.SessionPrefEffort
	if !strings.EqualFold(strings.TrimSpace(chatModel.ID), strings.TrimSpace(p.SessionPrefModelID)) {
		sessionEffort = ""
		previousModel := p.SessionPrefModelID
		if previousModel == "" {
			previousModel = botSettings.ChatModelID
		}
		if strings.TrimSpace(p.Model) != "" && !strings.EqualFold(strings.TrimSpace(chatModel.ID), strings.TrimSpace(previousModel)) {
			botSettings.ReasoningEffort = ""
		}
	}
	return resolveReasoningConfig(chatModel, botSettings, p.ReasoningEffort, sessionEffort, clientType)
}

// resolveReasoningConfig makes the single reasoning decision for a call. The
// decision itself lives in internal/reasoning, which also answers what a picker
// may offer — the two share internals so the options a user sees and the value a
// call sends cannot disagree.
//
// sessionEffort is the session's remembered tier (issue #879). It sits between
// the per-message request and the bot's stored value, implemented by shadowing
// `stored`: identical semantics to a dedicated level, and — critically — it is
// NOT passed as `requested`, because RunConfig.ReasoningRequestedEffort travels
// to spawned subagents as "the user's explicit pick this turn" and memory must
// not leak into that channel.
func resolveReasoningConfig(chatModel models.GetResponse, botSettings settings.Settings, requestedEffort, sessionEffort, clientType string) *models.ReasoningConfig {
	stored := botSettings.ReasoningEffort
	if e := strings.TrimSpace(sessionEffort); e != "" {
		stored = e
	}
	return reasoning.ResolveConfig(
		chatModel.ResolveThinkingMode(),
		chatModel.Config.ReasoningEfforts,
		chatModel.ReasoningOptions(clientType),
		stored,
		requestedEffort,
	)
}

func buildModelSelectionRequest(p baseParams, chatID string) runreq.ChatRequest {
	return runreq.ChatRequest{
		BotID:          p.BotID,
		ChatID:         chatID,
		ThreadID:       p.SessionID,
		CurrentChannel: p.CurrentPlatform,
		Model:          p.Model,
		Provider:       p.Provider,
	}
}

func normalizeGatewaySkill(entry SkillEntry) (native.SkillEntry, bool) {
	name := strings.TrimSpace(entry.Name)
	if name == "" {
		return native.SkillEntry{}, false
	}
	description := strings.TrimSpace(entry.Description)
	if description == "" {
		description = name
	}
	content := strings.TrimSpace(entry.Content)
	if content == "" {
		content = description
	}
	return native.SkillEntry{
		Name:        name,
		Description: description,
		Content:     content,
		Path:        strings.TrimSpace(entry.Path),
		Metadata:    entry.Metadata,
	}, true
}
