package runconfig

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/settings"
)

// errChatModelNotConfigured means no request, conversation, or bot model could be selected.
var errChatModelNotConfigured = errors.New("chat model not configured: specify model in request or bot settings")

func (b *Builder) selectChatModel(ctx context.Context, req runreq.ChatRequest, botSettings settings.Settings, sessionPrefModelID string) (models.GetResponse, sqlc.Provider, error) {
	modelID := strings.TrimSpace(req.Model)
	providerFilter := strings.TrimSpace(req.Provider)

	// Priority: request model > session preference (issue #879) > bot settings
	// > session history.
	if modelID == "" && providerFilter == "" {
		if value := strings.TrimSpace(sessionPrefModelID); value != "" {
			modelID = value
		} else if value := strings.TrimSpace(botSettings.ChatModelID); value != "" {
			modelID = value
		} else {
			// Resumed turns (ask_user answers, tool approval decisions) carry no
			// request model, and the bot may have no default chat model when the
			// web client selects the model per request. Continue with the model
			// that produced the session's latest round.
			modelID = b.latestSessionModelID(ctx, req.ThreadID)
		}
	}

	if modelID == "" {
		return models.GetResponse{}, sqlc.Provider{}, errChatModelNotConfigured
	}

	if providerFilter == "" {
		return b.FetchChatModel(ctx, modelID)
	}

	candidates, err := b.listCandidates(ctx, providerFilter)
	if err != nil {
		return models.GetResponse{}, sqlc.Provider{}, err
	}
	for _, m := range candidates {
		if matchesModelReference(m, modelID) {
			prov, err := models.FetchProviderByID(ctx, b.queries, m.ProviderID)
			if err != nil {
				return models.GetResponse{}, sqlc.Provider{}, err
			}
			if err := validateSelectedChatModel(m, prov); err != nil {
				return models.GetResponse{}, sqlc.Provider{}, err
			}
			if !prov.Enable {
				return models.GetResponse{}, sqlc.Provider{}, fmt.Errorf("chat model provider %s is disabled", prov.Name)
			}
			return m, prov, nil
		}
	}
	return models.GetResponse{}, sqlc.Provider{}, fmt.Errorf("chat model %q not found for provider %q", modelID, providerFilter)
}

// latestSessionModelID returns the models.id UUID of the most recent history
// message in the session that recorded one, or "" when the session has no
// model-bearing history yet.
func (b *Builder) latestSessionModelID(ctx context.Context, sessionID string) string {
	return models.LatestSessionModelID(ctx, b.queries, sessionID)
}

// sessionModelPreference loads the session's persisted (model, effort) pair
// (issue #879). Both components come back trimmed; empty means "no memory"
// and lets resolution fall through the chain. Missing session / unreadable
// id are treated as no memory, never as an error: the preference must not be
// able to break a turn.
func (b *Builder) sessionModelPreference(ctx context.Context, sessionID string) (string, string) {
	id, err := db.ParseUUID(sessionID)
	if err != nil {
		return "", ""
	}
	row, err := b.queries.GetSessionByID(ctx, id)
	if err != nil {
		return "", ""
	}
	modelID := ""
	if row.PreferredChatModelID.Valid {
		modelID = row.PreferredChatModelID.String()
	}
	effort := ""
	if row.PreferredReasoningEffort.Valid {
		effort = strings.TrimSpace(row.PreferredReasoningEffort.String)
	}
	// The pair is one value: a half pair (NULL model, surviving effort) arises
	// when ON DELETE SET NULL clears only the model column, and honoring the
	// effort alone would pin it onto whatever model the chain falls back to —
	// the cross-model effort memory the spec rules out (§3.7). The frontend
	// already treats a NULL model as no memory; match it here.
	if modelID == "" {
		effort = ""
	}
	return modelID, effort
}

// ReconcileSessionModelPreference validates and normalizes a candidate pair
// (issue #879, spec v2 §3.3): the model must exist on an enabled provider
// (empty ref falls back to the bot default); an illegal or empty effort
// silently resolves to the model's default tier, so the DB never stores an
// illegal pair (S6). Returns the model's UUID and the normalized effort.
// Shared by the picker PATCH and the first-send INSERT so both write points
// reconcile identically.
func (b *Builder) ReconcilePreference(ctx context.Context, botID, modelRef, effort string) (string, string, error) {
	modelID := strings.TrimSpace(modelRef)
	if modelID == "" {
		botSettings, err := b.bot.LoadSettings(ctx, botID)
		if err != nil {
			return "", "", err
		}
		modelID = strings.TrimSpace(botSettings.ChatModelID)
	}
	if modelID == "" {
		return "", "", invalidPreference(errors.New("chat model is required to set session model preference"))
	}
	chatModel, provider, err := b.FetchChatModel(ctx, modelID)
	if err != nil {
		return "", "", err
	}
	reconciled := ""
	if rc := resolveReasoningConfig(chatModel, settings.Settings{}, effort, "", provider.ClientType); rc != nil {
		switch {
		case rc.Active:
			reconciled = strings.TrimSpace(rc.Effort)
		case rc.Disabled:
			reconciled = "disable"
		}
	}
	return chatModel.ID, reconciled, nil
}

// compareAndSetModelPreference writes an already reconciled pair, only if the
// session's preference is still at expectedRevision.
func (b *Builder) CompareAndSetPreference(ctx context.Context, sessionUUID pgtype.UUID, runtimeType string, nativeID pgtype.UUID, externalID pgtype.Text, effort, expectedRevision string) error {
	revision, err := parsePreferenceRevision(expectedRevision)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrModelPreferenceRevisionInvalid, err)
	}
	n, err := b.queries.CompareAndSetSessionModelPreference(ctx, sqlc.CompareAndSetSessionModelPreferenceParams{
		ID: sessionUUID, RuntimeType: runtimeType, ExpectedRevision: revision,
		PreferredChatModelID: nativeID, PreferredExternalModelID: externalID,
		PreferredReasoningEffort: pgtype.Text{String: effort, Valid: effort != ""},
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrModelPreferenceConflict
	}
	return nil
}

func (b *Builder) FetchChatModel(ctx context.Context, modelID string) (models.GetResponse, sqlc.Provider, error) {
	modelRef := strings.TrimSpace(modelID)
	if modelRef == "" {
		return models.GetResponse{}, sqlc.Provider{}, invalidPreference(errors.New("model id is required"))
	}

	// Support both model UUID and model_id slug. UUID-formatted slugs still
	// work because we fall back to GetByModelID when UUID lookup misses.
	var model models.GetResponse
	var err error
	if _, parseErr := db.ParseUUID(modelRef); parseErr == nil {
		model, err = b.models.GetByID(ctx, modelRef)
		if err == nil {
			goto resolved
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return models.GetResponse{}, sqlc.Provider{}, err
		}
	}
	model, err = b.models.GetByModelID(ctx, modelRef)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return models.GetResponse{}, sqlc.Provider{}, invalidPreference(err)
		}
		return models.GetResponse{}, sqlc.Provider{}, err
	}

resolved:
	prov, err := models.FetchProviderByID(ctx, b.queries, model.ProviderID)
	if err != nil {
		return models.GetResponse{}, sqlc.Provider{}, err
	}
	if err := validateSelectedChatModel(model, prov); err != nil {
		return models.GetResponse{}, sqlc.Provider{}, invalidPreference(err)
	}
	if !prov.Enable {
		return models.GetResponse{}, sqlc.Provider{}, invalidPreference(fmt.Errorf("chat model provider %s is disabled", prov.Name))
	}
	return model, prov, nil
}

func validateSelectedChatModel(model models.GetResponse, provider sqlc.Provider) error {
	if model.Type != models.ModelTypeChat {
		return errors.New("model is not a chat model")
	}
	if !model.Enable {
		return fmt.Errorf("chat model %s is disabled", model.ModelID)
	}
	if isImageOnlyChatModel(model, provider) {
		return fmt.Errorf("chat model %s is an image generation model; configure it as the bot image model and use a chat model for conversation", model.ModelID)
	}
	return nil
}

func isImageOnlyChatModel(model models.GetResponse, provider sqlc.Provider) bool {
	return models.IsImageOnlyChatModel(model, provider)
}

func matchesModelReference(model models.GetResponse, modelRef string) bool {
	ref := strings.TrimSpace(modelRef)
	if ref == "" {
		return false
	}
	return model.ID == ref || model.ModelID == ref
}

func (b *Builder) listCandidates(ctx context.Context, providerFilter string) ([]models.GetResponse, error) {
	var all []models.GetResponse
	var err error
	if providerFilter != "" {
		all, err = b.models.ListEnabledByProviderClientType(ctx, models.ClientType(providerFilter))
	} else {
		all, err = b.models.ListEnabledByType(ctx, models.ModelTypeChat)
	}
	if err != nil {
		return nil, err
	}
	filtered := make([]models.GetResponse, 0, len(all))
	for _, m := range all {
		if m.Type == models.ModelTypeChat {
			filtered = append(filtered, m)
		}
	}
	return filtered, nil
}

// ErrModelPreferenceInvalid marks a model reference the caller can fix by
// picking another model: missing, unknown, disabled, or not a chat model.
// Store and settings failures never carry it.
var ErrModelPreferenceInvalid = errors.New("invalid model preference")

// ErrModelPreferenceRevisionInvalid means expected_model_preference_revision
// is not a UUID.
var ErrModelPreferenceRevisionInvalid = errors.New("invalid model preference revision")

// ErrACPPreferenceUnsupported means the session runs on ACP, whose model
// choice goes through the ACP runtime rather than the session preference.
var ErrACPPreferenceUnsupported = errors.New("ACP preferences must be changed through the ACP runtime")

type preferenceInvalidError struct{ err error }

func (e preferenceInvalidError) Error() string { return e.err.Error() }

func (e preferenceInvalidError) Unwrap() error { return e.err }

func (preferenceInvalidError) Is(target error) bool {
	return target == ErrModelPreferenceInvalid
}

func invalidPreference(err error) error { return preferenceInvalidError{err: err} }

// ErrModelPreferenceConflict means a picker read predates a newer write.
var ErrModelPreferenceConflict = errors.New("session model preference changed")

func parsePreferenceRevision(value string) (pgtype.UUID, error) {
	if strings.TrimSpace(value) == "" {
		return pgtype.UUID{}, nil
	}
	return db.ParseUUID(value)
}
