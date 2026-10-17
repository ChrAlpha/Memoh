package externalrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

var ErrDirectModelUnavailable = errors.New("direct runtime model unavailable")

// Direct runtimes use agent-owned model IDs, never models-table UUIDs. Keep
// their preference in a separate column so the native foreign key remains
// meaningful and runtime-owned conversation metadata is not read/modified.
func (h *Host) reconcilePreference(ctx context.Context, botID, botAgentID, runtimeType, projectPath, modelID, effort string) (string, string, error) {
	provider, ok := h.drivers[runtimeType].(external.ModelCatalogProvider)
	if !ok {
		return "", "", external.ErrModelCatalogUnavailable
	}
	catalog, err := provider.ModelCatalog(ctx, external.ModelCatalogRequest{
		BotID: botID, BotAgentID: botAgentID, ProjectPath: projectPath, ModelID: modelID, ResolveDefaults: strings.TrimSpace(modelID) == "",
	})
	if err != nil {
		return "", "", outcome.ExternalRuntimeError(err)
	}
	return reconcilePair(catalog, modelID, effort)
}

func reconcilePair(catalog external.ModelCatalog, modelID, effort string) (string, string, error) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		modelID = strings.TrimSpace(catalog.ConfiguredModelID)
	}
	if modelID == "" {
		for _, model := range catalog.Models {
			if model.Default {
				modelID = model.ID
				break
			}
		}
	}
	for _, model := range catalog.Models {
		if model.ID != modelID && (modelID == "" || model.ResolvedModelID != modelID) {
			continue
		}
		effort = strings.TrimSpace(effort)
		for _, option := range model.ReasoningEfforts {
			if effort != "" && option.ID == effort {
				return modelID, effort, nil
			}
		}
		// The model's declared default wins when changing models. Do not carry
		// another model's effort or substitute the agent-wide configured effort.
		for _, option := range model.ReasoningEfforts {
			if option.ID == model.DefaultReasoningEffort {
				return modelID, option.ID, nil
			}
		}
		return modelID, "", nil
	}
	return "", "", fmt.Errorf("%w: %q", ErrDirectModelUnavailable, modelID)
}

func (h *Host) applyModelPreference(ctx context.Context, req runreq.ChatRequest, sess session.Thread) (runreq.ChatRequest, error) {
	if !session.IsDirectRuntime(sess) || req.SessionType == sessionmode.Schedule {
		return req, nil
	}
	carries := strings.TrimSpace(req.Model) != "" || strings.TrimSpace(req.ReasoningEffort) != ""
	if !carries {
		req.Model = sess.PreferredExternalModelID
		if req.Model != "" {
			req.ReasoningEffort = sess.PreferredReasoningEffort
		}
		return req, nil
	}
	modelID := strings.TrimSpace(req.Model)
	if modelID == "" {
		modelID = sess.PreferredExternalModelID
	}
	effort := strings.TrimSpace(req.ReasoningEffort)
	// Catalog discovery is expensive (Claude Code spawns a CLI and waits for
	// initialize; Codex calls model/list or the custom /models endpoint). A
	// remembered Web session repeats its stored pair on every send, so only
	// re-validate when the request actually differs from what the session
	// already reconciled. The revision still advances below so an older picker
	// PATCH cannot overwrite this send.
	if modelID == "" || modelID != sess.PreferredExternalModelID || effort != sess.PreferredReasoningEffort {
		var err error
		modelID, effort, err = h.reconcilePreference(ctx, req.BotID, sess.BotAgentID, sess.RuntimeType, runreq.MetadataString(runreq.RuntimeSessionMeta(sess), "project_path"), modelID, effort)
		if err != nil {
			return req, err
		}
	}
	req.Model = modelID
	req.ReasoningEffort = effort
	// Best-effort like the native write-back: a preference write must not be
	// able to break a turn. The request pair still applies to this turn and
	// the next send retries the persist.
	err := h.store.UpdateSessionModelPreference(ctx, sqlc.UpdateSessionModelPreferenceParams{
		ID: db.ParseUUIDOrEmpty(sess.ID), PreferredExternalModelID: pgtype.Text{String: modelID, Valid: true}, PreferredReasoningEffort: pgtype.Text{String: effort, Valid: effort != ""},
	})
	if err != nil {
		h.logger.WarnContext(ctx, "write-back direct model preference",
			slog.String("session_id", sess.ID),
			slog.Any("error", err),
		)
	}
	return req, nil
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
func (h *Host) PatchSessionModelPreference(ctx context.Context, botID, sessionID string, modelRef, effort *string, expectedRevision string) error {
	sessionUUID, err := db.ParseUUID(sessionID)
	if err != nil {
		return fmt.Errorf("invalid session id: %w", err)
	}
	sess, err := h.store.GetSessionByID(ctx, sessionUUID)
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
	case session.IsDirectRuntimeType(sess.RuntimeType):
		if modelRef == nil || strings.TrimSpace(*modelRef) == "" {
			modelID = db.TextToString(sess.PreferredExternalModelID)
		}
		var thread session.Thread
		_ = json.Unmarshal(sess.Metadata, &thread.Metadata)
		_ = json.Unmarshal(sess.RuntimeMetadata, &thread.RuntimeMetadata)
		projectPath := runreq.MetadataString(runreq.RuntimeSessionMeta(thread), "project_path")
		modelID, reconciledEffort, err = h.reconcilePreference(ctx, sess.BotID.String(), sess.BotAgentID.String(), sess.RuntimeType, projectPath, modelID, targetEffort)
		externalID = pgtype.Text{String: modelID, Valid: modelID != ""}
	case sess.RuntimeType == session.RuntimeACPAgent:
		return runconfig.ErrACPPreferenceUnsupported
	default:
		modelID, reconciledEffort, err = h.runconfig.ReconcilePreference(ctx, botID, modelID, targetEffort)
		nativeID = db.ParseUUIDOrEmpty(modelID)
	}
	if err != nil {
		return err
	}
	return h.runconfig.CompareAndSetPreference(ctx, sessionUUID, sess.RuntimeType, nativeID, externalID, reconciledEffort, expectedRevision)
}
