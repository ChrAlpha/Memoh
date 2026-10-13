package message

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

// An invalidated artifact is read as an artifact of an older epoch: it is not
// a live summary, and the rows it covers are candidates for compaction again.
func TestPostgresInvalidatedArtifactIsNotReadAndItsRowsAreRecompacted(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)

	svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))
	user, err := svc.Persist(ctx, PersistInput{
		BotID:     postgresMessageTestBotID,
		SessionID: postgresMessageTestSessionID,
		Role:      "user",
		Content:   []byte(`{"role":"user","content":"hello"}`),
	})
	if err != nil {
		t.Fatalf("persist user: %v", err)
	}
	const artifactID = "00000000-0000-0000-0000-000000074141"
	if _, err := tx.Exec(ctx, `
		INSERT INTO bot_history_message_compacts (id, bot_id, session_id, status, summary, anchor_start_ms, anchor_end_ms)
		VALUES ($1, $2, $3, 'ok', 'covers the user row', 1, 2)
	`, artifactID, postgresMessageTestBotID, postgresMessageTestSessionID); err != nil {
		t.Fatalf("insert artifact: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE bot_history_messages SET compact_id = $1 WHERE id = $2`, artifactID, user.ID); err != nil {
		t.Fatalf("link user row: %v", err)
	}
	sessionID := mustTestUUID(t, postgresMessageTestSessionID)

	assertLiveSummaries := func(want int) {
		t.Helper()
		summaries, err := dbsqlc.New(tx).ListActiveCompactionSummariesBySession(ctx, sessionID)
		if err != nil {
			t.Fatalf("list active summaries: %v", err)
		}
		if len(summaries) != want {
			t.Fatalf("live summaries = %d, want %d", len(summaries), want)
		}
	}
	assertCandidates := func(want int) {
		t.Helper()
		rows, err := dbsqlc.New(tx).ListUncompactedMessagesBySessionWithinBytes(ctx, dbsqlc.ListUncompactedMessagesBySessionWithinBytesParams{
			SessionID: sessionID,
			MaxBytes:  1 << 30,
		})
		if err != nil {
			t.Fatalf("list uncompacted candidates: %v", err)
		}
		if len(rows) != want {
			t.Fatalf("compaction candidates = %d, want %d", len(rows), want)
		}
	}

	assertLiveSummaries(1)
	assertCandidates(0)

	if _, err := tx.Exec(ctx, `UPDATE bot_history_message_compacts SET invalidated_at = now() WHERE id = $1`, artifactID); err != nil {
		t.Fatalf("invalidate artifact: %v", err)
	}

	assertLiveSummaries(0)
	assertCandidates(1)
}

// A rollup may only take parents that are still valid. Parent selection drops
// an invalidated parent, and completion fails when one arrives after
// selection, so the rollup never folds in a summary of replaced history.
func TestPostgresRollupCompletionRejectsInvalidatedParent(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)

	const (
		validParentID       = "00000000-0000-0000-0000-000000074151"
		invalidatedParentID = "00000000-0000-0000-0000-000000074152"
	)
	if _, err := tx.Exec(ctx, `
		INSERT INTO bot_history_message_compacts (id, bot_id, session_id, status, summary, anchor_start_ms, anchor_end_ms, invalidated_at)
		VALUES
			($1, $3, $4, 'ok', 'valid parent', 1, 10, NULL),
			($2, $3, $4, 'ok', 'invalidated parent', 11, 20, now())
	`, validParentID, invalidatedParentID, postgresMessageTestBotID, postgresMessageTestSessionID); err != nil {
		t.Fatalf("insert parents: %v", err)
	}
	queries := dbsqlc.New(tx)
	complete := func(parents ...string) error {
		t.Helper()
		log, err := queries.CreateCompactionLog(ctx, dbsqlc.CreateCompactionLogParams{
			BotID:         mustTestUUID(t, postgresMessageTestBotID),
			SessionID:     mustTestUUID(t, postgresMessageTestSessionID),
			ExpectedEpoch: 0,
		})
		if err != nil {
			t.Fatalf("create rollup log: %v", err)
		}
		ids := make([]pgtype.UUID, 0, len(parents))
		for _, parent := range parents {
			ids = append(ids, mustTestUUID(t, parent))
		}
		_, err = queries.CompleteCompactionRollup(ctx, dbsqlc.CompleteCompactionRollupParams{
			ID:            log.ID,
			Status:        "ok",
			Summary:       "rollup",
			MessageCount:  0,
			Usage:         []byte("{}"),
			Coverage:      []byte("[]"),
			AnchorStartMs: 1,
			AnchorEndMs:   20,
			Level:         1,
			Parents:       ids,
		})
		return err
	}

	if err := complete(validParentID, invalidatedParentID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("rollup over an invalidated parent = %v, want pgx.ErrNoRows", err)
	}
	if err := complete(validParentID); err != nil {
		t.Fatalf("rollup over the valid parent only: %v", err)
	}
}

// A claim whose artifact was invalidated while it was in flight is reclaimable
// at once, without waiting for the lease to age out. The control subtest shows
// that a fresh claim of the same epoch is not reclaimable.
func TestPostgresInvalidatedPendingClaimIsReclaimable(t *testing.T) {
	tests := []struct {
		name       string
		invalidate bool
		wantMarked bool
	}{
		{"invalidated claim is reclaimed", true, true},
		{"fresh claim of the same epoch is held", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := setupCommittedCompactionFixture(t)
			ctx := context.Background()
			queries := dbsqlc.New(fixture.pool)
			first := fixture.createLog(t)
			second := fixture.createLog(t)
			messageIDs := fixtureMessageIDs(t, fixture)

			marked, err := queries.MarkMessagesCompacted(ctx, dbsqlc.MarkMessagesCompactedParams{
				CompactID:          first.ID,
				MessageIds:         messageIDs,
				ExpectedCompactIds: emptyCompactionClaims(len(messageIDs)),
			})
			if err != nil || marked != int64(len(messageIDs)) {
				t.Fatalf("first claim = %d, %v", marked, err)
			}
			if tc.invalidate {
				if _, err := fixture.pool.Exec(ctx, `
					UPDATE bot_history_message_compacts SET invalidated_at = now() WHERE id = $1
				`, first.ID); err != nil {
					t.Fatalf("invalidate first claim: %v", err)
				}
			}

			marked, err = queries.MarkMessagesCompacted(ctx, dbsqlc.MarkMessagesCompactedParams{
				CompactID:          second.ID,
				MessageIds:         messageIDs,
				ExpectedCompactIds: repeatedCompactionClaim(first.ID, len(messageIDs)),
			})
			if err != nil {
				t.Fatalf("second claim: %v", err)
			}
			if got := marked == int64(len(messageIDs)); got != tc.wantMarked {
				t.Fatalf("second claim marked %d rows, want reclaimed=%v", marked, tc.wantMarked)
			}
			if !tc.wantMarked {
				return
			}
			_, err = queries.CompleteCompactionLog(ctx, successfulCompaction(first.ID))
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("invalidated owner completion = %v, want pgx.ErrNoRows", err)
			}
		})
	}
}
