package memory

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	memprovider "github.com/felinics/memoh/internal/memory/adapters"
	"github.com/felinics/memoh/internal/models"
)

// OverviewStore reads what the refresh policy needs to tell whether a session's
// prompt cache is warm.
type OverviewStore interface {
	ListRecentContextLifecyclesBySession(context.Context, sqlc.ListRecentContextLifecyclesBySessionParams) ([]sqlc.ListRecentContextLifecyclesBySessionRow, error)
	ListActiveSessionRunsByBot(ctx context.Context, botID pgtype.UUID) ([]sqlc.SessionRun, error)
}

// OverviewDeps is everything NewOverview needs.
type OverviewDeps struct {
	// Logger receives the refresh's own records.
	Logger *slog.Logger
	// Store reads session runs.
	Store OverviewStore
	// Keeper finds the bot's memory provider.
	Keeper *Keeper
	// Now is the clock idle time and overview lag are measured with; nil
	// keeps time.Now.
	Now func() time.Time
}

// Overview decides when a bot's MEMORY.md is rewritten from its memory files,
// and remembers in this process which sessions are still inside their prompt
// cache.
type Overview struct {
	logger   *slog.Logger
	store    OverviewStore
	keeper   *Keeper
	now      func() time.Time
	activity sessionActivity
}

// NewOverview returns an Overview.
func NewOverview(d OverviewDeps) *Overview {
	o := &Overview{logger: d.Logger, store: d.Store, keeper: d.Keeper, now: d.Now}
	if o.now == nil {
		o.now = time.Now
	}
	return o
}

// memoryOverviewMaxLag bounds how long MEMORY.md may lag behind memory written
// during conversations. Past it, the next turn of any session rewrites
// MEMORY.md even while that session or another one still has a warm prompt
// cache, so a bot whose sessions never all go cold still sees its index catch
// up.
const memoryOverviewMaxLag = 30 * time.Minute

// memoryOverviewActivityHorizon is the longest prompt cache TTL; activity
// older than this can no longer keep any session warm. It must stay at least
// the longest TTL promptCacheTTLDuration returns, or a session still inside a
// longer cache would be pruned and treated as cold.
const memoryOverviewActivityHorizon = time.Hour

// Reasons a stale MEMORY.md is rewritten before the system prompt is built.
const (
	memoryOverviewRefreshNewSession   = "new_session"
	memoryOverviewRefreshCacheExpired = "cache_expired"
	memoryOverviewRefreshMaxLag       = "max_lag"
)

// Refresh applies the bot-wide overview freshness policy before
// loading system files. MEMORY.md sits in the system prompt ahead of the whole
// history, so rewriting it while a session is warm makes every following token
// of that session's next request miss the prompt cache. Normally it refreshes
// only at a cold turn, while no other session is warm or running, because the
// file is shared by every session of the bot. At the 30-minute lag limit it
// deliberately refreshes even warm sessions, trading cache reuse for a current
// index. Subagents never trigger a refresh: a subagent session is new by
// construction while its parent is warm, so the rewrite would break the
// parent's next request instead. New memories remain available through recall
// and memory tools while the shared index lags.
//
// TODO(memory): pin a MEMORY.md snapshot per session instead of deferring the
// shared file. The system prompt would carry the snapshot the session took at
// its last cold point (new session, after compaction, cache TTL expired, model
// switch, or too many pending updates), and memory written since then would
// reach the model as <memory-update> records persisted on the turn's request
// and replayed byte-identically, the way injectWorkspaceTransitionRecords
// replays workspace switches. One session's refresh would then never touch
// another's prompt, so MEMORY.md could again be rewritten on every write, the
// cross-session guard (process-local activity and the active-run lookup, with
// their restart and multi-instance blind spots) and the forced max-lag rewrite
// would go away, and subagents could inherit their parent's snapshot. It needs
// durable per-session state (a snapshot table keyed by content hash plus a
// memory view in the session metadata), so it is left to a migration of its
// own.
func (o *Overview) Refresh(ctx context.Context, botID, sessionID, sessionType, promptCacheTTL string) {
	botID = strings.TrimSpace(botID)
	sessionID = strings.TrimSpace(sessionID)
	if botID == "" || strings.EqualFold(strings.TrimSpace(sessionType), sessionmode.Subagent) {
		return
	}
	refresher, ok := o.keeper.Provider(ctx, botID).(memprovider.OverviewRefresher)
	if !ok {
		return
	}
	now := o.now()
	ttl := promptCacheTTLDuration(promptCacheTTL)
	defer o.activity.noteTurn(botID, sessionID, now, ttl)

	since, stale := refresher.OverviewStaleSince(botID)
	if !stale {
		return
	}
	// A bot the memory store had not seen starts lagging when first asked,
	// which is a moment after now.
	lag := max(now.Sub(since), 0)
	reason := memoryOverviewRefreshMaxLag
	if lag < memoryOverviewMaxLag {
		var cold bool
		if reason, cold = o.sessionPromptCacheCold(ctx, sessionID, now, ttl); !cold {
			return
		}
		if o.otherSessionWarm(ctx, botID, sessionID, now) {
			o.logger.DebugContext(ctx, "memory overview refresh deferred: another session may be warm",
				slog.String("bot_id", botID),
				slog.String("session_id", sessionID),
				slog.Duration("lag", lag),
			)
			return
		}
	}
	if err := refresher.RefreshOverview(ctx, botID); err != nil {
		o.logger.WarnContext(ctx, "memory overview refresh failed",
			slog.String("bot_id", botID),
			slog.String("session_id", sessionID),
			slog.String("reason", reason),
			slog.Any("error", err),
		)
		return
	}
	o.logger.DebugContext(ctx, "memory overview refreshed",
		slog.String("bot_id", botID),
		slog.String("session_id", sessionID),
		slog.String("reason", reason),
		slog.Duration("lag", lag),
	)
}

// sessionPromptCacheCold reports whether the session's prompt cache has
// expired: it has no finished run yet, or its last run ended at least ttl ago.
// A lookup failure counts as warm, which keeps the system prompt as it is.
func (o *Overview) sessionPromptCacheCold(ctx context.Context, sessionID string, now time.Time, ttl time.Duration) (string, bool) {
	if sessionID == "" {
		return memoryOverviewRefreshNewSession, true
	}
	sessionUUID, err := db.ParseUUID(sessionID)
	if err != nil {
		return "", false
	}
	rows, err := o.store.ListRecentContextLifecyclesBySession(ctx, sqlc.ListRecentContextLifecyclesBySessionParams{
		SessionID: sessionUUID,
		MaxCount:  1,
	})
	if err != nil {
		o.logger.WarnContext(ctx, "memory overview refresh: load last session run failed",
			slog.String("session_id", sessionID),
			slog.Any("error", err),
		)
		return "", false
	}
	if len(rows) == 0 || !rows[0].CreatedAt.Valid {
		return memoryOverviewRefreshNewSession, true
	}
	if now.Sub(rows[0].CreatedAt.Time) >= ttl {
		return memoryOverviewRefreshCacheExpired, true
	}
	return "", false
}

// otherSessionWarm uses each observed session's own TTL, plus the durable
// active-run list. Lookup failures defer refresh until the max-lag limit.
func (o *Overview) otherSessionWarm(ctx context.Context, botID, sessionID string, now time.Time) bool {
	if o.activity.warmOther(botID, sessionID, now) {
		return true
	}
	botUUID, err := db.ParseUUID(botID)
	if err != nil {
		return false
	}
	runs, err := o.store.ListActiveSessionRunsByBot(ctx, botUUID)
	if err != nil {
		o.logger.WarnContext(ctx, "memory overview refresh: load active runs failed",
			slog.String("bot_id", botID),
			slog.Any("error", err),
		)
		return true
	}
	self := db.ParseUUIDOrEmpty(sessionID)
	for _, run := range runs {
		if !self.Valid || run.SessionID.Bytes != self.Bytes {
			return true
		}
	}
	return false
}

// RunEnded records that a run of sessionID finished: its last provider request
// has just completed, which is what keeps the session's prompt cache warm.
func (o *Overview) RunEnded(botID, sessionID string) {
	o.activity.noteRunEnd(botID, sessionID, o.now())
}

// promptCacheTTLDuration is how long a provider is assumed to keep a prompt
// cache entry. A provider with prompt caching off still gets the default:
// providers with automatic prefix caching keep caching without Memoh's hints.
func promptCacheTTLDuration(ttl string) time.Duration {
	if models.NormalizePromptCacheTTL(strings.TrimSpace(ttl)) == models.PromptCacheTTL1h {
		return time.Hour
	}
	return 5 * time.Minute
}

// sessionActivity remembers when each session of a bot last started a turn or
// finished a run in this process, so a cold turn can tell whether rewriting
// the bot-wide MEMORY.md would break another session's warm prompt cache.
// Subagent sessions are never recorded.
type sessionActivity struct {
	mu   sync.Mutex
	last map[string]map[string]sessionCacheActivity
}

type sessionCacheActivity struct {
	last time.Time
	ttl  time.Duration
}

// noteTurn records that sessionID of botID is starting a turn at now.
func (a *sessionActivity) noteTurn(botID, sessionID string, now time.Time, ttl time.Duration) {
	if sessionID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.last == nil {
		a.last = make(map[string]map[string]sessionCacheActivity)
	}
	sessions := a.last[botID]
	if sessions == nil {
		sessions = make(map[string]sessionCacheActivity)
		a.last[botID] = sessions
	}
	sessions[sessionID] = sessionCacheActivity{last: now, ttl: ttl}
	pruneSessionActivity(sessions, now)
}

// noteRunEnd records that a run of sessionID finished at now. Only sessions
// that started a turn through Refresh are tracked, which
// keeps subagent sessions out.
func (a *sessionActivity) noteRunEnd(botID, sessionID string, now time.Time) {
	botID = strings.TrimSpace(botID)
	sessionID = strings.TrimSpace(sessionID)
	a.mu.Lock()
	defer a.mu.Unlock()
	if activity, tracked := a.last[botID][sessionID]; tracked {
		activity.last = now
		a.last[botID][sessionID] = activity
	}
}

// warmOther reports whether another session is within its own cache TTL.
func (a *sessionActivity) warmOther(botID, sessionID string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for other, activity := range a.last[botID] {
		if other != sessionID && now.Before(activity.last.Add(activity.ttl)) {
			return true
		}
	}
	return false
}

func pruneSessionActivity(sessions map[string]sessionCacheActivity, now time.Time) {
	for sessionID, activity := range sessions {
		if now.Sub(activity.last) > memoryOverviewActivityHorizon {
			delete(sessions, sessionID)
		}
	}
}
