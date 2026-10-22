package memory

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/transcript"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	memprovider "github.com/felinics/memoh/internal/memory/adapters"
	"github.com/felinics/memoh/internal/settings"
)

const (
	overviewTestSessionID      = "33333333-3333-3333-3333-333333333333"
	overviewTestOtherSessionID = "44444444-4444-4444-4444-444444444444"
)

type overviewRefreshProvider struct {
	memprovider.Provider
	stale      bool
	since      time.Time
	refreshErr error
	refreshes  int
}

func (*overviewRefreshProvider) Type() string { return "test" }

func (p *overviewRefreshProvider) OverviewStaleSince(string) (time.Time, bool) {
	return p.since, p.stale
}

func (p *overviewRefreshProvider) RefreshOverview(context.Context, string) error {
	p.refreshes++
	if p.refreshErr != nil {
		return p.refreshErr
	}
	p.stale = false
	return nil
}

type lastRunQueries struct {
	lastRun     time.Time
	err         error
	calls       int
	activeRuns  []string
	activeErr   error
	activeCalls int
}

func (q *lastRunQueries) ListActiveSessionRunsByBot(context.Context, pgtype.UUID) ([]sqlc.SessionRun, error) {
	q.activeCalls++
	if q.activeErr != nil {
		return nil, q.activeErr
	}
	runs := make([]sqlc.SessionRun, 0, len(q.activeRuns))
	for _, sessionID := range q.activeRuns {
		runs = append(runs, sqlc.SessionRun{SessionID: db.ParseUUIDOrEmpty(sessionID)})
	}
	return runs, nil
}

func (q *lastRunQueries) ListRecentContextLifecyclesBySession(_ context.Context, arg sqlc.ListRecentContextLifecyclesBySessionParams) ([]sqlc.ListRecentContextLifecyclesBySessionRow, error) {
	q.calls++
	if arg.MaxCount != 1 {
		return nil, errors.New("unexpected max_count")
	}
	if q.err != nil {
		return nil, q.err
	}
	if q.lastRun.IsZero() {
		return nil, nil
	}
	return []sqlc.ListRecentContextLifecyclesBySessionRow{{CreatedAt: pgtype.Timestamptz{Time: q.lastRun, Valid: true}}}, nil
}

func overviewRefresh(provider memprovider.Provider, store OverviewStore, now func() time.Time) *Overview {
	logger := slog.New(slog.DiscardHandler)
	registry := memprovider.NewRegistry(logger)
	registry.Register(agenttest.MemoryProviderID, provider)
	keeper := New(Deps{
		Logger:     logger,
		Settings:   settings.NewService(logger, &agenttest.MemorySettingsQueries{}, nil, nil),
		Bot:        agenttest.BotEnvWithSettings(logger, settings.NewService(logger, &agenttest.MemorySettingsQueries{}, nil, nil)),
		Transcript: transcript.New(transcript.Deps{Logger: logger}),
		Registry:   registry,
	})
	return NewOverview(OverviewDeps{Logger: logger, Store: store, Keeper: keeper, Now: now})
}

func TestRefreshMemoryOverview(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	cases := []struct {
		name        string
		stale       bool
		lag         time.Duration
		lastRun     time.Time
		lookupErr   error
		ttl         string
		sessionID   string
		sessionType string
		// otherActive is when another session of the bot last ran in this
		// process; zero means it never did.
		otherActive       time.Time
		otherTTL          string
		activeRuns        []string
		activeErr         error
		wantRefreshes     int
		wantLookups       int
		wantActiveLookups int
	}{
		{name: "fresh overview skips the lookup", stale: false, lastRun: now.Add(-time.Hour), sessionID: overviewTestSessionID},
		{name: "new session refreshes", stale: true, sessionID: overviewTestSessionID, wantRefreshes: 1, wantLookups: 1, wantActiveLookups: 1},
		{name: "warm session keeps the overview", stale: true, lastRun: now.Add(-time.Minute), sessionID: overviewTestSessionID, wantLookups: 1},
		{name: "expired default cache refreshes", stale: true, lastRun: now.Add(-6 * time.Minute), sessionID: overviewTestSessionID, wantRefreshes: 1, wantLookups: 1, wantActiveLookups: 1},
		{name: "one hour cache stays warm after six minutes", stale: true, lastRun: now.Add(-6 * time.Minute), ttl: "1h", sessionID: overviewTestSessionID, wantLookups: 1},
		{name: "one hour cache refreshes after an hour", stale: true, lastRun: now.Add(-61 * time.Minute), ttl: "1h", sessionID: overviewTestSessionID, wantRefreshes: 1, wantLookups: 1, wantActiveLookups: 1},
		{name: "cache off still waits for the default ttl", stale: true, lastRun: now.Add(-time.Minute), ttl: "off", sessionID: overviewTestSessionID, wantLookups: 1},
		{name: "lookup failure keeps the overview", stale: true, lookupErr: errors.New("db down"), sessionID: overviewTestSessionID, wantLookups: 1},
		{name: "sessionless run refreshes", stale: true, sessionID: "", wantRefreshes: 1, wantActiveLookups: 1},
		{name: "subagent session never refreshes", stale: true, sessionID: overviewTestSessionID, sessionType: "subagent"},
		{name: "another warm session defers the refresh", stale: true, sessionID: overviewTestSessionID, otherActive: now.Add(-2 * time.Minute), wantLookups: 1},
		{name: "another session idle past the ttl does not defer", stale: true, sessionID: overviewTestSessionID, otherActive: now.Add(-6 * time.Minute), wantRefreshes: 1, wantLookups: 1, wantActiveLookups: 1},
		{name: "5m caller preserves another session's 1h cache", stale: true, ttl: "5m", sessionID: overviewTestSessionID, otherActive: now.Add(-10 * time.Minute), otherTTL: "1h", wantLookups: 1},
		{name: "1h caller does not extend another session's expired 5m cache", stale: true, ttl: "1h", sessionID: overviewTestSessionID, otherActive: now.Add(-10 * time.Minute), otherTTL: "5m", wantRefreshes: 1, wantLookups: 1, wantActiveLookups: 1},
		{name: "other session expires exactly at its ttl", stale: true, sessionID: overviewTestSessionID, otherActive: now.Add(-5 * time.Minute), wantRefreshes: 1, wantLookups: 1, wantActiveLookups: 1},
		{name: "another session with a run in flight defers the refresh", stale: true, sessionID: overviewTestSessionID, activeRuns: []string{overviewTestOtherSessionID}, wantLookups: 1, wantActiveLookups: 1},
		{name: "the session's own run in flight does not defer", stale: true, sessionID: overviewTestSessionID, activeRuns: []string{overviewTestSessionID}, wantRefreshes: 1, wantLookups: 1, wantActiveLookups: 1},
		{name: "active run lookup failure defers the refresh", stale: true, sessionID: overviewTestSessionID, activeErr: errors.New("db down"), wantLookups: 1, wantActiveLookups: 1},
		{name: "lag under the cap still defers for another warm session", stale: true, lag: memoryOverviewMaxLag - time.Minute, sessionID: overviewTestSessionID, otherActive: now.Add(-time.Minute), wantLookups: 1},
		{name: "lag past the cap refreshes a warm session", stale: true, lag: memoryOverviewMaxLag, lastRun: now.Add(-time.Minute), sessionID: overviewTestSessionID, wantRefreshes: 1},
		{name: "lag past the cap overrides another warm session", stale: true, lag: memoryOverviewMaxLag + time.Minute, sessionID: overviewTestSessionID, otherActive: now.Add(-time.Minute), activeRuns: []string{overviewTestOtherSessionID}, wantRefreshes: 1},
		{name: "lag past the cap still skips subagent sessions", stale: true, lag: time.Hour, sessionID: overviewTestSessionID, sessionType: "subagent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &overviewRefreshProvider{stale: tc.stale, since: now.Add(-tc.lag)}
			queries := &lastRunQueries{lastRun: tc.lastRun, err: tc.lookupErr, activeRuns: tc.activeRuns, activeErr: tc.activeErr}
			service := overviewRefresh(provider, queries, clock)
			if !tc.otherActive.IsZero() {
				service.activity.noteTurn(agenttest.MemoryBotID, overviewTestOtherSessionID, tc.otherActive, promptCacheTTLDuration(tc.otherTTL))
			}

			service.Refresh(context.Background(), agenttest.MemoryBotID, tc.sessionID, tc.sessionType, tc.ttl)

			if provider.refreshes != tc.wantRefreshes {
				t.Fatalf("refreshes = %d, want %d", provider.refreshes, tc.wantRefreshes)
			}
			if queries.calls != tc.wantLookups {
				t.Fatalf("last-run lookups = %d, want %d", queries.calls, tc.wantLookups)
			}
			if queries.activeCalls != tc.wantActiveLookups {
				t.Fatalf("active-run lookups = %d, want %d", queries.activeCalls, tc.wantActiveLookups)
			}
		})
	}
}

func TestRefreshMemoryOverviewWaitsForAnotherSessionThatJustRan(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	provider := &overviewRefreshProvider{stale: true, since: now}
	// Every session's own last run looks recent for the first turn below and
	// absent afterwards, so only the cross-session check decides.
	queries := &lastRunQueries{lastRun: now.Add(-time.Minute)}
	service := overviewRefresh(provider, queries, clock)

	// A warm turn of the first session leaves MEMORY.md alone but records the
	// session as active.
	service.Refresh(context.Background(), agenttest.MemoryBotID, overviewTestOtherSessionID, "", "")
	// A new session four minutes later must not break the first one's cache.
	now = now.Add(4 * time.Minute)
	queries.lastRun = time.Time{}
	service.Refresh(context.Background(), agenttest.MemoryBotID, overviewTestSessionID, "", "")
	if provider.refreshes != 0 {
		t.Fatalf("refreshes = %d, want the new session to wait for the warm one", provider.refreshes)
	}
	// Once both sessions have been idle past the TTL, a cold turn refreshes.
	now = now.Add(6 * time.Minute)
	service.Refresh(context.Background(), agenttest.MemoryBotID, "55555555-5555-5555-5555-555555555555", "", "")
	if provider.refreshes != 1 {
		t.Fatalf("refreshes = %d, want one once no other session is warm", provider.refreshes)
	}
}

func TestSessionActivityRunEndOnlyUpdatesTrackedSessions(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	var activity sessionActivity
	activity.noteTurn(agenttest.MemoryBotID, overviewTestOtherSessionID, start, time.Hour)

	activity.noteRunEnd(agenttest.MemoryBotID, overviewTestOtherSessionID, start.Add(10*time.Minute))
	activity.noteRunEnd(agenttest.MemoryBotID, "subagent-session", start.Add(10*time.Minute))

	if !activity.warmOther(agenttest.MemoryBotID, overviewTestSessionID, start.Add(50*time.Minute)) {
		t.Fatal("a run that ended after a long turn must keep its session warm from the run's end")
	}
	if activity.warmOther(agenttest.MemoryBotID, overviewTestSessionID, start.Add(70*time.Minute)) {
		t.Fatal("the session must expire one TTL after run end")
	}
	if activity.warmOther(agenttest.MemoryBotID, overviewTestOtherSessionID, start) {
		t.Fatal("a session never counts as warm against itself")
	}
	if _, tracked := activity.last[agenttest.MemoryBotID]["subagent-session"]; tracked {
		t.Fatal("run end of a session that never started a turn here must not be tracked")
	}
}

func TestSessionActivityPrunesSessionsPastTheLongestTTL(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	var activity sessionActivity
	activity.noteTurn(agenttest.MemoryBotID, overviewTestOtherSessionID, start, 5*time.Minute)
	activity.noteTurn(agenttest.MemoryBotID, overviewTestSessionID, start.Add(memoryOverviewActivityHorizon+time.Minute), 5*time.Minute)

	if _, tracked := activity.last[agenttest.MemoryBotID][overviewTestOtherSessionID]; tracked {
		t.Fatal("activity older than the longest prompt cache TTL should be pruned")
	}
}

func TestRefreshMemoryOverviewSurvivesRefreshFailure(t *testing.T) {
	t.Parallel()

	provider := &overviewRefreshProvider{stale: true, refreshErr: errors.New("workspace unavailable")}
	service := overviewRefresh(provider, &lastRunQueries{}, nil)

	service.Refresh(context.Background(), agenttest.MemoryBotID, overviewTestSessionID, "", "")

	if provider.refreshes != 1 || !provider.stale {
		t.Fatalf("refreshes = %d stale = %v, want one failed attempt that leaves the overview stale", provider.refreshes, provider.stale)
	}
}

func TestRefreshMemoryOverviewIgnoresProvidersWithoutRefresher(t *testing.T) {
	t.Parallel()

	queries := &lastRunQueries{}
	service := overviewRefresh(&agenttest.MemoryProvider{}, queries, nil)

	service.Refresh(context.Background(), agenttest.MemoryBotID, overviewTestSessionID, "", "")

	if queries.calls != 0 {
		t.Fatalf("last-run lookups = %d, want none for a provider that never defers MEMORY.md", queries.calls)
	}
}
