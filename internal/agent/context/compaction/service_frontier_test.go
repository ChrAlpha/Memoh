package compaction

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

func TestDoCompactionRefusesALegacySummaryWithoutSourceRows(t *testing.T) {
	rows := machineryCorpus(t)
	stub := &stubModel{summary: "S2"}
	cfg := machineryConfig(stub, 450)
	q := &fakeQueries{
		uncompacted: rows,
		priorLogs: []sqlc.BotHistoryMessageCompact{{
			ID:        pgtype.UUID{Bytes: uuid.New(), Valid: true},
			BotID:     pgtype.UUID{Bytes: uuid.MustParse(cfg.BotID), Valid: true},
			SessionID: pgtype.UUID{Bytes: uuid.MustParse(cfg.SessionID), Valid: true},
			Summary:   "earlier-segment-summary",
			Status:    "ok",
		}},
	}
	svc := newMachineryService(q)

	_, err := svc.RunCompactionSync(context.Background(), cfg)
	if !errors.Is(err, errIncompleteAbsorbedCoverage) || stub.calls != 0 || q.created || len(q.backfills) != 0 {
		t.Fatalf("err=%v calls=%d created=%t backfills=%d, want a refusal before any claim", err, stub.calls, q.created, len(q.backfills))
	}
}

func TestDoCompactionAbsorbsOnlyTheActiveArtifactFrontier(t *testing.T) {
	parentID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	activeID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	stub := &stubModel{summary: "S2"}
	cfg := machineryConfig(stub, 450)
	botID := pgtype.UUID{Bytes: uuid.MustParse(cfg.BotID), Valid: true}
	sessionID := pgtype.UUID{Bytes: uuid.MustParse(cfg.SessionID), Valid: true}
	coverage := testCoverageJSON(t, "covered-row")
	q := &fakeQueries{
		uncompacted: machineryCorpus(t),
		priorLogs: []sqlc.BotHistoryMessageCompact{
			{
				ID:           parentID,
				BotID:        botID,
				SessionID:    sessionID,
				Status:       "ok",
				Summary:      "stale-parent-summary",
				Coverage:     coverage,
				SupersededBy: activeID,
				SupersededAt: pgtype.Timestamptz{Time: time.Unix(1, 0), Valid: true},
			},
			{
				ID:        activeID,
				BotID:     botID,
				SessionID: sessionID,
				Status:    "ok",
				Summary:   "active-frontier-summary",
				Coverage:  coverage,
				ParentIds: []pgtype.UUID{parentID},
			},
		},
	}

	if _, err := newMachineryService(q).RunCompactionSync(context.Background(), cfg); err != nil {
		t.Fatalf("RunCompactionSync: %v", err)
	}
	if !strings.Contains(stub.prompt, "active-frontier-summary") || strings.Contains(stub.prompt, "stale-parent-summary") {
		t.Fatalf("absorbed context did not use the active frontier:\n%s", stub.prompt)
	}
	if parents := q.rollupCalls[0].Parents; len(parents) != 1 || parents[0] != activeID {
		t.Fatalf("rollup parents = %v, want only the active artifact", parents)
	}
}
