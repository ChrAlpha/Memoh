package compaction

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/chat/checkpoint"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

func (s *Service) captureSourceVersions(ctx context.Context, cfg TriggerConfig, items []CompactionCandidate) error {
	var ids []pgtype.UUID
	for _, item := range items {
		if item.Record.ExternalMessageID != "" && item.Record.EventID != "" {
			ids = append(ids, db.ParseUUIDOrEmpty(item.Record.EventID))
		}
	}
	cursors := make(map[string]int64)
	if len(ids) > 0 {
		rows, err := s.queries.ListSessionEventCursors(ctx, sqlc.ListSessionEventCursorsParams{SessionID: db.ParseUUIDOrEmpty(cfg.SessionID), Ids: ids})
		if err != nil {
			return err
		}
		for _, row := range rows {
			cursors[row.ID.String()] = row.EventCursor
		}
	}
	for i, item := range items {
		items[i].EventCursor = cursors[item.Record.EventID]
		for _, source := range cfg.View {
			changed := source.Kind == "history" && source.ID == formatUUID(item.ID) && source.ContentHash != "" && source.ContentHash != historyfrag.MessageContentHash(item.Record.ModelMessage)
			changed = changed || source.Kind == "external" && source.ID == item.Record.ExternalMessageID && (source.EventCursor <= 0 || source.EventCursor != items[i].EventCursor)
			if changed {
				return failed(ReasonStaleSource, fmt.Errorf("%w: source %s differs from the request view", errStaleSource, source.ID))
			}
		}
	}
	return nil
}

// matchView refuses a pass whose request renders other summaries than the
// checkpoint it folds onto holds: the request was composed on a checkpoint
// the session no longer has, so no fold onto the active one matches it. A
// caller without a view renders the active checkpoint.
func matchView(cfg TriggerConfig, parent checkpoint.Checkpoint) error {
	if cfg.View == nil {
		return nil
	}
	rendered := make(map[string]bool)
	for _, source := range cfg.View {
		if source.Kind == "summary" {
			rendered[source.ID] = true
		}
	}
	held := parent.SummaryIDs()
	matches := len(rendered) == len(held)
	for _, id := range held {
		matches = matches && rendered[id]
	}
	if !matches {
		return failed(ReasonStaleSource, fmt.Errorf("%w: the request renders summaries the active checkpoint %q does not hold", errStaleSource, parent.ID))
	}
	return nil
}
