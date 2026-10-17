package application

import (
	"context"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

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
