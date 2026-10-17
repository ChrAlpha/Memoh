package application

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/models"
)

// writeBackSessionModelPreference persists the resolved pair only when the
// request carries one. Every carried send advances the revision, even when
// its value matches, so an older picker PATCH cannot overwrite that send.
// Writes remain best-effort: failures are logged and the next send retries.
func (s *Service) writeBackSessionModelPreference(ctx context.Context, sessionIDRaw string, carriesPair bool, chatModel models.GetResponse, reasoning *models.ReasoningConfig) {
	if !carriesPair {
		return
	}
	sessionID, err := db.ParseUUID(sessionIDRaw)
	if err != nil {
		return
	}
	modelID, err := db.ParseUUID(chatModel.ID)
	if err != nil {
		return
	}
	effort := ""
	if reasoning != nil {
		switch {
		case reasoning.Active:
			effort = strings.TrimSpace(reasoning.Effort)
		case reasoning.Disabled:
			// Same vocabulary as bot settings ("disable" = thinking off) so a
			// seeded picker round-trips the user's choice instead of falling
			// back to the model's default-on tier.
			effort = "disable"
		}
	}
	// Every carried send advances the revision, including an unchanged pair.
	// Otherwise a picker still comparing against this revision could overwrite it.
	if err := s.queries.UpdateSessionModelPreference(ctx, sqlc.UpdateSessionModelPreferenceParams{
		ID:                       sessionID,
		PreferredChatModelID:     modelID,
		PreferredReasoningEffort: pgtype.Text{String: effort, Valid: effort != ""},
	}); err != nil {
		s.logger.WarnContext(ctx, "write-back session model preference",
			slog.String("session_id", sessionIDRaw),
			slog.Any("error", err),
		)
	}
}

// PatchSessionModelPreference handles the picker PATCH (issue #879, spec
// §3.3 / §5-2): reconcile the pair and write it. Reconcile = the effort must
// be legal for the target model; an illegal or empty tier silently resolves
// to the model default, so the DB never stores an illegal pair.
//
// Target model resolution when the request omits it: existing session
// preference, then bot default. Effort-only patches therefore reconcile
// against the model the session would actually use. An explicit but
// unresolvable/empty model reference is an error (the FK would reject it
// anyway; fail loudly instead of degrading).
//
// expectedRevision is the revision the picker read; "" means the session has
// none yet. Every PATCH is compare-and-set: a picker may never overwrite a
// send or a newer picker operation it did not see.
func (s *Service) PatchSessionModelPreference(ctx context.Context, botID, sessionID string, modelRef, effort *string, expectedRevision string) error {
	sessionUUID, err := db.ParseUUID(sessionID)
	if err != nil {
		return fmt.Errorf("invalid session id: %w", err)
	}
	sess, err := s.queries.GetSessionByID(ctx, sessionUUID)
	if err != nil {
		return err
	}

	modelID := ""
	if modelRef != nil {
		modelID = strings.TrimSpace(*modelRef)
	}
	if modelID == "" && sess.PreferredChatModelID.Valid {
		modelID = sess.PreferredChatModelID.String()
	}
	targetEffort := ""
	if effort != nil {
		targetEffort = strings.TrimSpace(*effort)
	} else if (modelRef == nil || strings.TrimSpace(*modelRef) == "") && sess.PreferredReasoningEffort.Valid {
		targetEffort = strings.TrimSpace(sess.PreferredReasoningEffort.String)
	}

	var nativeID pgtype.UUID
	var externalID pgtype.Text
	var reconciledEffort string
	switch {
	case sessionpkg.IsDirectRuntimeType(sess.RuntimeType):
		if modelRef == nil || strings.TrimSpace(*modelRef) == "" {
			modelID = db.TextToString(sess.PreferredExternalModelID)
		}
		var thread sessionpkg.Thread
		_ = json.Unmarshal(sess.Metadata, &thread.Metadata)
		_ = json.Unmarshal(sess.RuntimeMetadata, &thread.RuntimeMetadata)
		projectPath := runreq.MetadataString(runreq.RuntimeSessionMeta(thread), "project_path")
		modelID, reconciledEffort, err = s.reconcileDirectModelPreference(ctx, sess.BotID.String(), sess.BotAgentID.String(), sess.RuntimeType, projectPath, modelID, targetEffort)
		externalID = pgtype.Text{String: modelID, Valid: modelID != ""}
	case sess.RuntimeType == sessionpkg.RuntimeACPAgent:
		return runconfig.ErrACPPreferenceUnsupported
	default:
		modelID, reconciledEffort, err = s.runconfig.ReconcilePreference(ctx, botID, modelID, targetEffort)
		nativeID = db.ParseUUIDOrEmpty(modelID)
	}
	if err != nil {
		return err
	}
	return s.runconfig.CompareAndSetPreference(ctx, sessionUUID, sess.RuntimeType, nativeID, externalID, reconciledEffort, expectedRevision)
}
