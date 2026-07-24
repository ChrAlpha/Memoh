package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	session "github.com/memohai/memoh/internal/chat/thread"
	"github.com/memohai/memoh/internal/db"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
)

const (
	contextLifecycleDefaultLimit = 50
	contextLifecycleMaxLimit     = 200
)

type ContextLifecycleResponse struct {
	Turns      []ContextLifecycleTurn     `json:"turns"`
	Aggregates ContextLifecycleAggregates `json:"aggregates"`
}

// ContextLifecycleTurn is one persisted lifecycle snapshot, newest first.
type ContextLifecycleTurn struct {
	MessageID string                        `json:"message_id"`
	CreatedAt time.Time                     `json:"created_at"`
	Snapshot  contextfrag.LifecycleSnapshot `json:"snapshot"`
}

type ContextLifecycleAggregates struct {
	Turns                 int            `json:"turns"`
	CacheOutcomes         map[string]int `json:"cache_outcomes,omitempty"`
	CacheHitRate          float64        `json:"cache_hit_rate"`
	TotalCacheReadTokens  int            `json:"total_cache_read_tokens"`
	TotalCacheWriteTokens int            `json:"total_cache_write_tokens"`
	DropReasons           map[string]int `json:"drop_reasons,omitempty"`
	MutationKinds         map[string]int `json:"mutation_kinds,omitempty"`
}

// GetSessionContextLifecycle godoc
// @Summary Get session context lifecycle
// @Description List the persisted context lifecycle snapshots (selection, cache plan, mutations, cache attribution) for a chat session with aggregated cache and drop statistics
// @Tags sessions
// @Param bot_id path string true "Bot ID"
// @Param session_id path string true "Session ID"
// @Param limit query int false "Maximum number of turns to return (default 50, max 200)"
// @Success 200 {object} ContextLifecycleResponse
// @Failure 400 {object} ErrorResponse
// @Failure 403 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /bots/{bot_id}/sessions/{session_id}/context-lifecycle [get].
func (h *SessionInfoHandler) GetSessionContextLifecycle(c echo.Context) error {
	userID, err := RequireChannelIdentityID(c)
	if err != nil {
		return err
	}
	botID := strings.TrimSpace(c.Param("bot_id"))
	if botID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "bot id is required")
	}
	sessionID := strings.TrimSpace(c.Param("session_id"))
	if sessionID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "session id is required")
	}
	pgSessionID, err := db.ParseUUID(sessionID)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid session id")
	}

	ctx := c.Request().Context()
	sessionRow, err := h.queries.GetSessionByID(ctx, pgSessionID)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "session not found")
	}
	sessionMode, runtimeType := normalizedSessionDescriptor(session.Thread{
		Type:        sessionRow.Type,
		SessionMode: sessionRow.SessionMode,
		RuntimeType: sessionRow.RuntimeType,
	})
	bot, err := AuthorizeBotAccessWithPermission(ctx, h.botService, h.accountService, userID, botID, requiredReadPermissionForSessionRuntime(sessionMode, runtimeType))
	if err != nil {
		return err
	}
	if sessionRow.BotID.String() != bot.ID {
		return echo.NewHTTPError(http.StatusNotFound, "session not found")
	}
	perms, err := h.resolveCurrentUserPermissions(c, userID, bot.ID)
	if err != nil {
		return err
	}
	sess := session.Thread{
		ID:          sessionRow.ID.String(),
		BotID:       sessionRow.BotID.String(),
		Type:        sessionRow.Type,
		SessionMode: sessionMode,
		RuntimeType: runtimeType,
	}
	if sessionRow.CreatedByUserID.Valid {
		sess.CreatedByUserID = sessionRow.CreatedByUserID.String()
	}
	if !canAccessSession(sess, userID, perms) {
		return echo.NewHTTPError(http.StatusNotFound, "session not found")
	}

	limit := contextLifecycleLimit(c)
	rows, err := h.queries.ListRecentAssistantMessagesBySession(ctx, sqlc.ListRecentAssistantMessagesBySessionParams{
		SessionID: pgSessionID,
		MaxCount:  int32(limit), //nolint:gosec // G115: limit is bounded to contextLifecycleMaxLimit
	})
	if err != nil {
		h.logger.Error("list session messages failed", slog.Any("error", err))
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to load session messages")
	}

	turns := lifecycleTurnsFromRows(rows, limit)
	return c.JSON(http.StatusOK, ContextLifecycleResponse{
		Turns:      turns,
		Aggregates: aggregateContextLifecycle(turns),
	})
}

func contextLifecycleLimit(c echo.Context) int {
	limit := contextLifecycleDefaultLimit
	if raw := strings.TrimSpace(c.QueryParam("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > contextLifecycleMaxLimit {
		limit = contextLifecycleMaxLimit
	}
	return limit
}

// lifecycleTurnsFromRows extracts persisted lifecycle snapshots from
// assistant message metadata, newest first, bounded by limit.
func lifecycleTurnsFromRows(rows []sqlc.ListRecentAssistantMessagesBySessionRow, limit int) []ContextLifecycleTurn {
	turns := make([]ContextLifecycleTurn, 0, limit)
	for _, row := range rows {
		if len(turns) >= limit {
			break
		}
		if len(row.Metadata) == 0 {
			continue
		}
		var metadata struct {
			ContextLifecycle *contextfrag.LifecycleSnapshot `json:"context_lifecycle"`
		}
		if json.Unmarshal(row.Metadata, &metadata) != nil || metadata.ContextLifecycle == nil {
			continue
		}
		turns = append(turns, ContextLifecycleTurn{
			MessageID: row.ID.String(),
			CreatedAt: row.CreatedAt.Time,
			Snapshot:  *metadata.ContextLifecycle,
		})
	}
	return turns
}

func aggregateContextLifecycle(turns []ContextLifecycleTurn) ContextLifecycleAggregates {
	agg := ContextLifecycleAggregates{Turns: len(turns)}
	comparableTurns := 0
	hits := 0
	for _, turn := range turns {
		agg.TotalCacheReadTokens += turn.Snapshot.CacheReadTokens
		agg.TotalCacheWriteTokens += turn.Snapshot.CacheWriteTokens
		if comparison := turn.Snapshot.CacheComparison; comparison != nil {
			if agg.CacheOutcomes == nil {
				agg.CacheOutcomes = make(map[string]int, 4)
			}
			agg.CacheOutcomes[comparison.Outcome]++
			if comparison.Outcome != contextfrag.CacheOutcomeFirstObservation {
				comparableTurns++
				if comparison.Outcome == contextfrag.CacheOutcomeHit {
					hits++
				}
			}
		}
		for reason, count := range turn.Snapshot.Selection.DropReasons {
			if agg.DropReasons == nil {
				agg.DropReasons = make(map[string]int, 4)
			}
			agg.DropReasons[reason] += count
		}
		for _, record := range turn.Snapshot.Mutations {
			if agg.MutationKinds == nil {
				agg.MutationKinds = make(map[string]int, 4)
			}
			agg.MutationKinds[string(record.Kind)]++
		}
	}
	if comparableTurns > 0 {
		agg.CacheHitRate = float64(hits) / float64(comparableTurns) * 100
	}
	return agg
}
