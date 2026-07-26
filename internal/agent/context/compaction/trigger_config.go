package compaction

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/memohai/memoh/internal/db"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/memohai/memoh/internal/db/store"
	"github.com/memohai/memoh/internal/models"
	"github.com/memohai/memoh/internal/providers"
)

// Sentinel resolution failures. Automatic triggers map them to a silent skip;
// manual surfaces (HTTP, slash command) translate them into user-facing errors.
var (
	ErrTriggerModelNotConfigured     = errors.New("no compaction or chat model configured")
	ErrTriggerModelNotChat           = errors.New("compaction model is not a chat model")
	ErrTriggerModelDisabled          = errors.New("compaction model is disabled")
	ErrTriggerOutputLimitUnsupported = errors.New("compaction model provider does not enforce the output limit")
)

// TriggerModelSource resolves model records; *models.Service satisfies it.
type TriggerModelSource interface {
	GetByID(ctx context.Context, id string) (models.GetResponse, error)
}

// TriggerCredentialSource resolves provider credentials; *providers.Service
// satisfies it.
type TriggerCredentialSource interface {
	ResolveModelCredentials(ctx context.Context, provider sqlc.Provider) (providers.ModelCredentials, error)
}

// ResolveTriggerConfig resolves the model, provider, credentials, and both
// token budgets shared by every compaction entry point. Model priority is the
// chat chain: explicit compaction override, then the bot's default chat
// model, then the model that produced the session's latest round — so bots
// whose model is chosen per request still compact with the model actually in
// use. Both budgets derive from the resolved model's context window:
// MaxCompactTokens caps candidate selection at 90% of the window and
// ContextTokenBudget carries the full window for final envelope accounting —
// a config without it would fail closed before the summarizer call.
func ResolveTriggerConfig(
	ctx context.Context,
	modelSource TriggerModelSource,
	queries dbstore.Queries,
	credentials TriggerCredentialSource,
	compactionModelID string,
	chatModelID string,
	sessionID string,
) (TriggerConfig, error) {
	modelID := strings.TrimSpace(compactionModelID)
	if modelID == "" {
		modelID = strings.TrimSpace(chatModelID)
	}
	if modelID == "" {
		modelID = latestSessionModelID(ctx, queries, sessionID)
	}
	if modelID == "" {
		return TriggerConfig{}, ErrTriggerModelNotConfigured
	}
	compactModel, err := modelSource.GetByID(ctx, modelID)
	if err != nil {
		return TriggerConfig{}, fmt.Errorf("resolve compaction model: %w", err)
	}
	if compactModel.Type != models.ModelTypeChat {
		return TriggerConfig{}, fmt.Errorf("%w: %s", ErrTriggerModelNotChat, compactModel.ModelID)
	}
	if !compactModel.Enable {
		return TriggerConfig{}, fmt.Errorf("%w: %s", ErrTriggerModelDisabled, compactModel.ModelID)
	}
	provider, err := models.FetchProviderByID(ctx, queries, compactModel.ProviderID)
	if err != nil {
		return TriggerConfig{}, fmt.Errorf("resolve compaction provider: %w", err)
	}
	if !models.EnforcesMaxOutputTokens(models.ClientType(strings.TrimSpace(provider.ClientType))) {
		return TriggerConfig{}, ErrTriggerOutputLimitUnsupported
	}
	creds, err := credentials.ResolveModelCredentials(ctx, provider)
	if err != nil {
		return TriggerConfig{}, fmt.Errorf("resolve compaction credentials: %w", err)
	}
	cfg := TriggerConfig{
		ModelID:        compactModel.ModelID,
		ModelRecordID:  compactModel.ID,
		ClientType:     provider.ClientType,
		APIKey:         creds.APIKey,
		CodexAccountID: creds.CodexAccountID,
		BaseURL:        providers.ProviderConfigString(provider, "base_url"),
		PromptCacheTTL: providers.ProviderConfigString(provider, "prompt_cache_ttl"),
	}
	if compactModel.Config.ContextWindow != nil && *compactModel.Config.ContextWindow > 0 {
		window := *compactModel.Config.ContextWindow
		cfg.SummaryWindowTokens = window
		cfg.MaxCompactTokens = window * 90 / 100
	}
	return cfg, nil
}

// IsTriggerConfigUnavailable reports whether the resolution failure means
// "this bot cannot compact right now" rather than an infrastructure error.
func IsTriggerConfigUnavailable(err error) bool {
	return errors.Is(err, ErrTriggerModelNotConfigured) ||
		errors.Is(err, ErrTriggerModelNotChat) ||
		errors.Is(err, ErrTriggerModelDisabled) ||
		errors.Is(err, ErrTriggerOutputLimitUnsupported)
}

func latestSessionModelID(ctx context.Context, queries dbstore.Queries, sessionID string) string {
	if queries == nil {
		return ""
	}
	parsed, err := db.ParseUUID(sessionID)
	if err != nil {
		return ""
	}
	modelID, err := queries.GetLatestSessionModelID(ctx, parsed)
	if err != nil || !modelID.Valid {
		return ""
	}
	return modelID.String()
}
