package command

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	dbsqlc "github.com/memohai/memoh/internal/db/postgres/sqlc"
)

// buildContextGroup registers /context — a focused token/context-window view for
// the current session (a Claude-Code-style budget card). It reuses the same
// session queries as /status; it is session-scoped, so the channel processor
// resolves the active session before dispatching (see isStatusCommand).
func (h *Handler) buildContextGroup() *CommandGroup {
	g := newCommandGroup("context", "Show context window usage")
	g.DefaultAction = "show"
	g.Register(SubCommand{
		Name:  "show",
		Usage: "show - Show context window usage for the current session",
		Handler: func(cc CommandContext) (string, error) {
			if strings.TrimSpace(cc.SessionID) == "" {
				return cc.T("cmd.session.noActive"), nil
			}
			return h.renderContextUsage(cc, cc.SessionID)
		},
	})
	return g
}

func (h *Handler) renderContextUsage(cc CommandContext, sessionID string) (string, error) {
	if h.queries == nil {
		return cc.T("cmd.session.unavailable"), nil
	}
	pgSessionID, err := parseCommandUUID(sessionID)
	if err != nil {
		return "", err
	}
	used, err := h.queries.GetLatestAssistantUsage(cc.Ctx, pgSessionID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("get usage: %w", err)
	}
	msgCount, err := h.queries.CountMessagesBySession(cc.Ctx, pgSessionID)
	if err != nil {
		return "", fmt.Errorf("count messages: %w", err)
	}
	cacheRow, _ := h.queries.GetSessionCacheStats(cc.Ctx, pgSessionID)
	var cacheHit float64
	if cacheRow.TotalInputTokens > 0 {
		cacheHit = float64(cacheRow.CacheReadTokens) / float64(cacheRow.TotalInputTokens) * 100
	}
	window := h.resolveContextWindowTokens(cc)

	var b strings.Builder
	b.WriteString(MdBold(cc.T("cmd.context.title")))
	b.WriteString("\n\n")
	if window > 0 {
		frac := float64(used) / float64(window)
		fmt.Fprintf(&b, "%s  %s", renderProgressBar(frac, 12), cc.T("cmd.context.usedWithWindow", map[string]any{
			"percent": fmt.Sprintf("%.0f%%", frac*100),
			"used":    formatTokens(used),
			"window":  formatTokens(window),
		}))
	} else {
		fmt.Fprintf(&b, "%s", cc.T("cmd.context.tokensUsed", map[string]any{"used": formatTokens(used)}))
	}
	for _, row := range h.contextCompositionRows(cc, pgSessionID) {
		fmt.Fprintf(&b, "\n- %s: ~%s", row.label, formatTokens(int64(row.tokens)))
	}
	fmt.Fprintf(&b, "\n\n- %s: %d", cc.T("cmd.status.fieldMessages"), msgCount)
	if cacheRow.TotalInputTokens > 0 {
		fmt.Fprintf(&b, "\n- %s: %.1f%%", cc.T("cmd.context.fieldCacheHit"), cacheHit)
	}
	return b.String(), nil
}

type contextCompositionRow struct {
	label  string
	tokens int
}

// contextCompositionRows renders the latest persisted lifecycle snapshot as
// per-category estimate rows: the by-kind breakdown plus tool definition
// buckets, largest first. Sessions without a snapshot render no rows.
func (h *Handler) contextCompositionRows(cc CommandContext, sessionID pgtype.UUID) []contextCompositionRow {
	rows, err := h.queries.ListRecentAssistantMessagesBySession(cc.Ctx, dbsqlc.ListRecentAssistantMessagesBySessionParams{
		SessionID: sessionID,
		MaxCount:  1,
	})
	if err != nil || len(rows) == 0 {
		return nil
	}
	snapshot, ok := contextfrag.LifecycleSnapshotFromMetadata(rows[0].Metadata)
	if !ok {
		return nil
	}
	out := make([]contextCompositionRow, 0, len(snapshot.Breakdown)+2)
	for _, entry := range snapshot.Breakdown {
		if entry.TokenEstimate <= 0 {
			continue
		}
		out = append(out, contextCompositionRow{label: contextKindLabel(cc, entry.Kind), tokens: entry.TokenEstimate})
	}
	buckets := map[string]int{}
	for _, def := range snapshot.ToolDefs {
		buckets[def.Provider] += def.TokenEstimate
	}
	for provider, tokens := range buckets {
		if tokens <= 0 {
			continue
		}
		label := provider
		switch provider {
		case "mcp":
			label = cc.T("cmd.context.mcpTools")
		case "native":
			label = cc.T("cmd.context.toolDefs")
		}
		out = append(out, contextCompositionRow{label: label, tokens: tokens})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].tokens != out[j].tokens {
			return out[i].tokens > out[j].tokens
		}
		return out[i].label < out[j].label
	})
	return out
}

// contextKindLabel localizes a fragment kind for the /context rows, falling
// back to the raw kind slug for kinds without a translation entry.
func contextKindLabel(cc CommandContext, kind contextfrag.Kind) string {
	key := "cmd.context.kind." + string(kind)
	if label := cc.T(key); label != key {
		return label
	}
	return string(kind)
}

// renderProgressBar draws a fixed-width unicode bar (█ filled, ░ empty). The
// glyphs are plain unicode and survive both the Telegram HTML pass and the
// plain-text strip unchanged.
func renderProgressBar(frac float64, cells int) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac*float64(cells) + 0.5)
	if filled > cells {
		filled = cells
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", cells-filled)
}
