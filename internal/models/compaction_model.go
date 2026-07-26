package models

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/memohai/memoh/internal/db"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/memohai/memoh/internal/db/store"
)

// Compaction model resolution failures. Automatic triggers map them to a
// silent skip; manual surfaces translate them into user-facing errors.
var (
	ErrCompactionModelNotConfigured     = errors.New("no compaction or chat model configured")
	ErrCompactionModelNotChat           = errors.New("compaction model is not a chat model")
	ErrCompactionModelDisabled          = errors.New("compaction model is disabled")
	ErrCompactionProviderDisabled       = errors.New("compaction model provider is disabled")
	ErrCompactionOutputLimitUnsupported = errors.New("compaction model provider does not enforce the output limit")
	ErrCompactionWindowUnknown          = errors.New("compaction model does not declare a context window")
)

// CompactionModelResolution is the resolved summarizer identity. Credentials
// stay with the caller: orchestration surfaces own auth context, the
// compaction engine only receives a completed contract.
type CompactionModelResolution struct {
	Model    GetResponse
	Provider sqlc.Provider
	// WindowTokens is the summarizer model's declared context window.
	WindowTokens int
}

// ResolveCompactionModel picks the first non-empty candidate model id and
// validates that it can act as a summarizer: a chat-type, enabled model on an
// enabled LLM provider that honors output caps and declares a context window
// (the summary budget derives from it, so an unknown window fails closed).
// Callers compose the candidate chain: automatic triggers pass the override
// and the turn's actually-resolved model; manual surfaces prefer the
// session's latest model before the bot default.
func ResolveCompactionModel(
	ctx context.Context,
	modelsService *Service,
	queries dbstore.Queries,
	candidates ...string,
) (CompactionModelResolution, error) {
	modelID := ""
	for _, candidate := range candidates {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			modelID = candidate
			break
		}
	}
	if modelID == "" {
		return CompactionModelResolution{}, ErrCompactionModelNotConfigured
	}
	model, err := modelsService.GetByID(ctx, modelID)
	if err != nil {
		return CompactionModelResolution{}, fmt.Errorf("resolve compaction model: %w", err)
	}
	if model.Type != ModelTypeChat {
		return CompactionModelResolution{}, fmt.Errorf("%w: %s", ErrCompactionModelNotChat, model.ModelID)
	}
	if !model.Enable {
		return CompactionModelResolution{}, fmt.Errorf("%w: %s", ErrCompactionModelDisabled, model.ModelID)
	}
	provider, err := FetchProviderByID(ctx, queries, model.ProviderID)
	if err != nil {
		return CompactionModelResolution{}, fmt.Errorf("resolve compaction provider: %w", err)
	}
	if !provider.Enable {
		return CompactionModelResolution{}, fmt.Errorf("%w: %s", ErrCompactionProviderDisabled, provider.Name)
	}
	clientType := ClientType(strings.TrimSpace(provider.ClientType))
	if !IsLLMClientType(clientType) {
		return CompactionModelResolution{}, fmt.Errorf("%w: %s", ErrCompactionModelNotChat, provider.ClientType)
	}
	if !EnforcesMaxOutputTokens(clientType) {
		return CompactionModelResolution{}, ErrCompactionOutputLimitUnsupported
	}
	if model.Config.ContextWindow == nil || *model.Config.ContextWindow <= 0 {
		return CompactionModelResolution{}, fmt.Errorf("%w: %s", ErrCompactionWindowUnknown, model.ModelID)
	}
	return CompactionModelResolution{
		Model:        model,
		Provider:     provider,
		WindowTokens: *model.Config.ContextWindow,
	}, nil
}

// IsCompactionModelUnavailable reports whether the resolution failure means
// "this bot cannot compact right now" rather than an infrastructure error.
func IsCompactionModelUnavailable(err error) bool {
	return errors.Is(err, ErrCompactionModelNotConfigured) ||
		errors.Is(err, ErrCompactionModelNotChat) ||
		errors.Is(err, ErrCompactionModelDisabled) ||
		errors.Is(err, ErrCompactionProviderDisabled) ||
		errors.Is(err, ErrCompactionOutputLimitUnsupported) ||
		errors.Is(err, ErrCompactionWindowUnknown)
}

// LatestSessionModelID returns the models.id UUID of the most recent history
// message in the session that recorded one, or "" when the session has no
// model-bearing history yet.
func LatestSessionModelID(ctx context.Context, queries dbstore.Queries, sessionID string) string {
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
