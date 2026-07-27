package handlers

import (
	"log/slog"
	"net/http"
	"sort"
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
	// TotalExpectedStableTokens sums the plan-time stable prefix estimates of
	// turns that had a chance to hit (first observations excluded), and
	// CacheReadEfficiency is the share of that offer the provider actually
	// read — a hit rate says breakpoints matched, this says how much of the
	// prefix they recovered.
	TotalExpectedStableTokens int                `json:"total_expected_stable_tokens"`
	CacheReadEfficiency       float64            `json:"cache_read_efficiency"`
	DropReasons               map[string]int     `json:"drop_reasons,omitempty"`
	MutationKinds             map[string]int     `json:"mutation_kinds,omitempty"`
	ToolRosterChanges         int                `json:"tool_roster_changes"`
	ToolRosterChangeDetails   []ToolRosterChange `json:"tool_roster_change_details,omitempty"`
}

// ToolRosterChange records how a turn's serialized tool roster differs from
// the previous turn's. Tools sit first in the provider cache order, so any
// entry here invalidates every cached prefix behind it.
type ToolRosterChange struct {
	MessageID string   `json:"message_id"`
	Added     []string `json:"added,omitempty"`
	Removed   []string `json:"removed,omitempty"`
	Resized   []string `json:"resized,omitempty"`
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
		snapshot, ok := contextfrag.LifecycleSnapshotFromMetadata(row.Metadata)
		if !ok {
			continue
		}
		turns = append(turns, ContextLifecycleTurn{
			MessageID: row.ID.String(),
			CreatedAt: row.CreatedAt.Time,
			Snapshot:  snapshot,
		})
	}
	return turns
}

// latestContextComposition projects the newest turn's snapshot into the
// status panel shape: the by-kind breakdown as persisted, and tool
// definitions rolled up per provider bucket, largest first.
func latestContextComposition(turns []ContextLifecycleTurn) ([]contextfrag.KindBreakdown, []ToolDefBucket) {
	if len(turns) == 0 {
		return nil, nil
	}
	snapshot := turns[0].Snapshot
	var buckets []ToolDefBucket
	if len(snapshot.ToolDefs) > 0 {
		byProvider := make(map[string]*ToolDefBucket, 2)
		for _, def := range snapshot.ToolDefs {
			bucket, ok := byProvider[def.Provider]
			if !ok {
				bucket = &ToolDefBucket{Provider: def.Provider}
				byProvider[def.Provider] = bucket
			}
			bucket.Tools++
			bucket.TokenEstimate += def.TokenEstimate
		}
		buckets = make([]ToolDefBucket, 0, len(byProvider))
		for _, bucket := range byProvider {
			buckets = append(buckets, *bucket)
		}
		sort.Slice(buckets, func(i, j int) bool {
			if buckets[i].TokenEstimate != buckets[j].TokenEstimate {
				return buckets[i].TokenEstimate > buckets[j].TokenEstimate
			}
			return buckets[i].Provider < buckets[j].Provider
		})
	}
	return snapshot.Breakdown, buckets
}

func aggregateContextLifecycle(turns []ContextLifecycleTurn) ContextLifecycleAggregates {
	agg := ContextLifecycleAggregates{Turns: len(turns)}
	comparableTurns := 0
	hits := 0
	comparableReadTokens := 0
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
				comparableReadTokens += turn.Snapshot.CacheReadTokens
				agg.TotalExpectedStableTokens += turn.Snapshot.StablePrefixTokenEstimate
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
	if agg.TotalExpectedStableTokens > 0 {
		agg.CacheReadEfficiency = float64(comparableReadTokens) / float64(agg.TotalExpectedStableTokens) * 100
	}
	agg.ToolRosterChanges, agg.ToolRosterChangeDetails = toolRosterChurn(turns)
	return agg
}

const toolRosterChangeDetailCap = 10

// toolRosterChurn diffs each turn's tool roster against the previous turn's
// (turns arrive newest first). Turns where either side carries no accounting
// are skipped so pre-accounting history does not read as a full roster swap.
func toolRosterChurn(turns []ContextLifecycleTurn) (int, []ToolRosterChange) {
	changes := 0
	var details []ToolRosterChange
	for i := 0; i+1 < len(turns); i++ {
		current, previous := turns[i], turns[i+1]
		if len(current.Snapshot.ToolDefs) == 0 || len(previous.Snapshot.ToolDefs) == 0 {
			continue
		}
		change := diffToolRosters(previous.Snapshot.ToolDefs, current.Snapshot.ToolDefs)
		if len(change.Added) == 0 && len(change.Removed) == 0 && len(change.Resized) == 0 {
			continue
		}
		change.MessageID = current.MessageID
		changes++
		if len(details) < toolRosterChangeDetailCap {
			details = append(details, change)
		}
	}
	return changes, details
}

func diffToolRosters(previous, current []contextfrag.ToolDefAccounting) ToolRosterChange {
	key := func(def contextfrag.ToolDefAccounting) string { return def.Provider + "/" + def.Name }
	prevBytes := make(map[string]int, len(previous))
	for _, def := range previous {
		prevBytes[key(def)] = def.Bytes
	}
	var change ToolRosterChange
	seen := make(map[string]bool, len(current))
	for _, def := range current {
		k := key(def)
		seen[k] = true
		before, existed := prevBytes[k]
		switch {
		case !existed:
			change.Added = append(change.Added, k)
		case before != def.Bytes:
			change.Resized = append(change.Resized, k)
		}
	}
	for _, def := range previous {
		if k := key(def); !seen[k] {
			change.Removed = append(change.Removed, k)
		}
	}
	sort.Strings(change.Added)
	sort.Strings(change.Removed)
	sort.Strings(change.Resized)
	return change
}
