package message

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbpkg "github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

// Hiding, deleting, or superseding history invalidates the artifacts covering
// the changed rows and leaves the compaction epoch where it is.
func TestPostgresHistoryMutationsInvalidateCoveringArtifactsWithoutEpochBump(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(context.Context, pgx.Tx, claimedCompactionFixture) error
	}{
		{
			name: "supersede turn",
			mutate: func(ctx context.Context, tx pgx.Tx, fixture claimedCompactionFixture) error {
				queries := postgresstore.NewQueries(dbsqlc.New(tx))
				_, err := queries.SupersedeHistoryTurn(ctx, dbsqlc.SupersedeHistoryTurnParams{
					SupersededByTurnID: mustTestUUID(t, uuid.NewString()),
					SupersededAt:       pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
					SupersededReason:   pgtype.Text{String: "test", Valid: true},
					OldTurnID:          mustTestUUID(t, fixture.turn.ID),
					SessionID:          mustTestUUID(t, postgresMessageTestSessionID),
				})
				return err
			},
		},
		{
			name: "hide turn",
			mutate: func(ctx context.Context, tx pgx.Tx, fixture claimedCompactionFixture) error {
				return postgresstore.NewQueries(dbsqlc.New(tx)).HideMessagesByHistoryTurn(ctx, mustTestUUID(t, fixture.turn.ID))
			},
		},
		{
			name: "delete source",
			mutate: func(ctx context.Context, tx pgx.Tx, fixture claimedCompactionFixture) error {
				return postgresstore.NewQueries(dbsqlc.New(tx)).DeleteMessagesByIDs(ctx, []pgtype.UUID{mustTestUUID(t, fixture.assistant.ID)})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			tx := beginPostgresMessageTestTx(t, ctx)
			setupPostgresMessageTestFixtures(t, ctx, tx)
			fixture := setupClaimedCompactionFixture(t, ctx, tx)

			if err := test.mutate(ctx, tx, fixture); err != nil {
				t.Fatalf("mutate claimed history: %v", err)
			}
			assertSessionCompactionEpoch(t, ctx, tx, 0)
			assertArtifactInvalidated(t, ctx, tx, fixture.artifactID, true)

			lineage, err := dbsqlc.New(tx).ListCompactionArtifactLineageBySession(ctx, mustTestUUID(t, postgresMessageTestSessionID))
			if err != nil {
				t.Fatalf("list active compaction lineage: %v", err)
			}
			if len(lineage) != 0 {
				t.Fatalf("invalidated compaction lineage remained active: %#v", lineage)
			}
		})
	}
}

// An artifact covering rows of the hidden turn is invalidated as a whole, even
// when it also covers rows outside that turn. Those rows return to the
// candidates; an artifact over the other turn stays live.
func TestPostgresHideInvalidatesPartiallyCoveringArtifact(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)
	sessionID := mustTestUUID(t, postgresMessageTestSessionID)

	user1, assistant1, turn1 := persistPostgresTurn(t, ctx, NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx))))
	user2, assistant2, _ := persistPostgresTurn(t, ctx, NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx))))

	const partialID = "00000000-0000-0000-0000-000000075101"
	const turnTwoID = "00000000-0000-0000-0000-000000075102"
	insertCompactArtifact(t, ctx, tx, partialID, "spans both turns", 0, 1, 2, nil, nil)
	insertCompactArtifact(t, ctx, tx, turnTwoID, "covers the second turn", 0, 3, 4, nil, nil)
	linkCompactSources(t, ctx, tx, partialID, user1.ID, assistant1.ID, user2.ID)
	linkCompactSources(t, ctx, tx, turnTwoID, assistant2.ID)

	assertLiveSummaries(t, ctx, tx, sessionID, []string{"spans both turns", "covers the second turn"})
	assertCompactionCandidates(t, ctx, tx, sessionID, 0)

	if err := postgresstore.NewQueries(dbsqlc.New(tx)).HideMessagesByHistoryTurn(ctx, mustTestUUID(t, turn1.ID)); err != nil {
		t.Fatalf("hide first turn: %v", err)
	}

	assertSessionCompactionEpoch(t, ctx, tx, 0)
	assertArtifactInvalidated(t, ctx, tx, partialID, true)
	assertArtifactInvalidated(t, ctx, tx, turnTwoID, false)
	assertLiveSummaries(t, ctx, tx, sessionID, []string{"covers the second turn"})
	assertCompactionCandidates(t, ctx, tx, sessionID, 1)
}

// A rollup is derived from its parents, so invalidating the rollup that covers
// the hidden rows also invalidates the parent covering them. The other parent
// covers no hidden row: it stays valid and becomes live again.
func TestPostgresHideInvalidatesRollupAndReleasesItsOtherParents(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)
	sessionID := mustTestUUID(t, postgresMessageTestSessionID)
	queries := postgresstore.NewQueries(dbsqlc.New(tx))

	user1, _, turn1 := persistPostgresTurn(t, ctx, NewService(nil, queries))
	user2, _, _ := persistPostgresTurn(t, ctx, NewService(nil, queries))

	const hiddenLeafID = "00000000-0000-0000-0000-000000075201"
	const keptLeafID = "00000000-0000-0000-0000-000000075202"
	const rollupID = "00000000-0000-0000-0000-000000075203"
	insertCompactArtifact(t, ctx, tx, hiddenLeafID, "hidden leaf", 0, 1, 2, nil, nil)
	insertCompactArtifact(t, ctx, tx, keptLeafID, "kept leaf", 0, 3, 4, nil, nil)
	insertCompactArtifact(t, ctx, tx, rollupID, "rollup", 1, 1, 4, []string{hiddenLeafID, keptLeafID}, nil)
	if _, err := tx.Exec(ctx, `
		UPDATE bot_history_message_compacts
		SET superseded_by = $1, superseded_at = now()
		WHERE id = ANY($2::uuid[])
	`, rollupID, []string{hiddenLeafID, keptLeafID}); err != nil {
		t.Fatalf("supersede parents: %v", err)
	}
	linkCompactSources(t, ctx, tx, hiddenLeafID, user1.ID)
	linkCompactSources(t, ctx, tx, keptLeafID, user2.ID)

	assertLiveSummaries(t, ctx, tx, sessionID, []string{"rollup"})

	if err := postgresstore.NewQueries(dbsqlc.New(tx)).HideMessagesByHistoryTurn(ctx, mustTestUUID(t, turn1.ID)); err != nil {
		t.Fatalf("hide first turn: %v", err)
	}

	assertSessionCompactionEpoch(t, ctx, tx, 0)
	assertArtifactInvalidated(t, ctx, tx, rollupID, true)
	assertArtifactInvalidated(t, ctx, tx, hiddenLeafID, true)
	assertArtifactInvalidated(t, ctx, tx, keptLeafID, false)
	assertLiveSummaries(t, ctx, tx, sessionID, []string{"kept leaf"})
}

// ClearHistory discards the whole session, so it keeps advancing the epoch.
func TestPostgresClearHistoryStillAdvancesCompactionEpoch(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)
	setupClaimedCompactionFixture(t, ctx, tx)

	if err := dbsqlc.New(tx).ClearHistoryBySession(ctx, mustTestUUID(t, postgresMessageTestSessionID)); err != nil {
		t.Fatalf("clear session history: %v", err)
	}
	assertSessionCompactionEpoch(t, ctx, tx, 1)
	assertLiveSummaries(t, ctx, tx, mustTestUUID(t, postgresMessageTestSessionID), nil)
}

// A retry invalidates the artifact crossing the cutoff and the rollup above it.
// The rollup's other parents end before the cutoff and cover rows the retry
// keeps; they must come back as live summaries, not stay superseded.
func TestPostgresRetryReleasesParentsOfInvalidatedRollup(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)
	svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))
	sessionID := mustTestUUID(t, postgresMessageTestSessionID)

	earlierUser, earlierAssistant, _ := persistPostgresTurn(t, ctx, svc)
	user, assistant, oldTurn := persistPostgresTurn(t, ctx, svc)
	var cutoffMs int64
	if err := tx.QueryRow(ctx, `
		SELECT FLOOR(EXTRACT(EPOCH FROM created_at) * 1000)::bigint FROM bot_history_messages WHERE id = $1
	`, assistant.ID).Scan(&cutoffMs); err != nil {
		t.Fatalf("read assistant created_at: %v", err)
	}

	const leftID = "00000000-0000-0000-0000-000000075161"
	const rightID = "00000000-0000-0000-0000-000000075162"
	const crossingID = "00000000-0000-0000-0000-000000075163"
	const rollupID = "00000000-0000-0000-0000-000000075164"
	insertCompactArtifact(t, ctx, tx, leftID, "left of the turn", 0, 1, cutoffMs-2, nil, nil)
	insertCompactArtifact(t, ctx, tx, rightID, "right of the turn", 0, 3, cutoffMs-1, nil, nil)
	insertCompactArtifact(t, ctx, tx, crossingID, "crosses the cutoff", 0, 1, cutoffMs, nil, nil)
	insertCompactArtifact(t, ctx, tx, rollupID, "rollup", 1, 1, cutoffMs, []string{leftID, rightID, crossingID}, nil)
	if _, err := tx.Exec(ctx, `
		UPDATE bot_history_message_compacts
		SET superseded_by = $1, superseded_at = now()
		WHERE id = ANY($2::uuid[])
	`, rollupID, []string{leftID, rightID, crossingID}); err != nil {
		t.Fatalf("supersede parents: %v", err)
	}
	linkCompactSources(t, ctx, tx, leftID, earlierUser.ID)
	linkCompactSources(t, ctx, tx, rightID, earlierAssistant.ID)
	linkCompactSources(t, ctx, tx, crossingID, assistant.ID)
	assertLiveSummaries(t, ctx, tx, sessionID, []string{"rollup"})

	replacement, err := svc.Persist(ctx, PersistInput{
		BotID:           postgresMessageTestBotID,
		SessionID:       postgresMessageTestSessionID,
		Role:            "assistant",
		Content:         []byte(`{"role":"assistant","content":"new"}`),
		SkipHistoryTurn: true,
	})
	if err != nil {
		t.Fatalf("persist replacement assistant: %v", err)
	}
	replacementTurnID, replacementTurnPosition := postgresReplacementIdentity(oldTurn)
	if _, err := svc.ReplaceTurn(ctx, postgresMessageTestSessionID, oldTurn.ID, replacementTurnID, replacementTurnPosition, user.ID, replacement.ID, "retry"); err != nil {
		t.Fatalf("replace turn: %v", err)
	}

	assertSessionCompactionEpoch(t, ctx, tx, 0)
	assertArtifactInvalidated(t, ctx, tx, crossingID, true)
	assertArtifactInvalidated(t, ctx, tx, rollupID, true)
	assertArtifactInvalidated(t, ctx, tx, leftID, false)
	assertArtifactInvalidated(t, ctx, tx, rightID, false)
	assertLiveSummaries(t, ctx, tx, sessionID, []string{"left of the turn", "right of the turn"})
}

func TestPostgresStaleCompactionAttemptCannotCompleteOK(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)

	svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))
	user, assistant, turn := persistPostgresTurn(t, ctx, svc)
	queries := postgresstore.NewQueries(dbsqlc.New(tx))
	log, err := queries.CreateCompactionLog(ctx, dbsqlc.CreateCompactionLogParams{
		BotID:     mustTestUUID(t, postgresMessageTestBotID),
		SessionID: mustTestUUID(t, postgresMessageTestSessionID),
	})
	if err != nil {
		t.Fatalf("create compaction log: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE bot_history_messages
		SET compact_id = $1
		WHERE id = ANY($2::uuid[])
	`, log.ID, []string{user.ID, assistant.ID}); err != nil {
		t.Fatalf("claim compaction sources: %v", err)
	}
	if err := queries.HideMessagesByHistoryTurn(ctx, mustTestUUID(t, turn.ID)); err != nil {
		t.Fatalf("hide claimed turn: %v", err)
	}
	assertSessionCompactionEpoch(t, ctx, tx, 0)

	complete := dbsqlc.CompleteCompactionLogParams{
		ID:           log.ID,
		Status:       "ok",
		Summary:      "stale summary",
		MessageCount: 2,
		Usage:        []byte(`{}`),
		Coverage:     []byte(`[]`),
	}
	if _, err := queries.CompleteCompactionLog(ctx, complete); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("complete claim over hidden rows = %v, want pgx.ErrNoRows", err)
	}

	complete.Status = "error"
	complete.Summary = ""
	complete.ErrorMessage = "history source changed"
	row, err := queries.CompleteCompactionLog(ctx, complete)
	if err != nil {
		t.Fatalf("record stale compaction error: %v", err)
	}
	if row.Status != "error" {
		t.Fatalf("stale compaction status = %q, want error", row.Status)
	}
}

type claimedCompactionFixture struct {
	user       Message
	assistant  Message
	turn       HistoryTurn
	artifactID string
}

func setupClaimedCompactionFixture(t *testing.T, ctx context.Context, tx pgx.Tx) claimedCompactionFixture {
	t.Helper()
	svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))
	user, assistant, turn := persistPostgresTurn(t, ctx, svc)
	artifactID := uuid.NewString()
	if _, err := tx.Exec(ctx, `
		INSERT INTO bot_history_message_compacts (
			id, bot_id, session_id, status, summary, message_count, completed_at
		)
		VALUES ($1, $2, $3, 'ok', 'claimed summary', 2, now())
	`, artifactID, postgresMessageTestBotID, postgresMessageTestSessionID); err != nil {
		t.Fatalf("insert compaction artifact: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE bot_history_messages
		SET compact_id = $1
		WHERE id = ANY($2::uuid[])
	`, artifactID, []string{user.ID, assistant.ID}); err != nil {
		t.Fatalf("link compaction sources: %v", err)
	}
	return claimedCompactionFixture{user: user, assistant: assistant, turn: turn, artifactID: artifactID}
}

func persistPostgresTurn(t *testing.T, ctx context.Context, svc *DBService) (Message, Message, HistoryTurn) {
	t.Helper()
	user, err := svc.Persist(ctx, PersistInput{
		BotID:     postgresMessageTestBotID,
		SessionID: postgresMessageTestSessionID,
		Role:      "user",
		Content:   []byte(`{"role":"user","content":"hello"}`),
	})
	if err != nil {
		t.Fatalf("persist user: %v", err)
	}
	assistant, err := svc.Persist(ctx, PersistInput{
		BotID:     postgresMessageTestBotID,
		SessionID: postgresMessageTestSessionID,
		Role:      "assistant",
		Content:   []byte(`{"role":"assistant","content":"answer"}`),
	})
	if err != nil {
		t.Fatalf("persist assistant: %v", err)
	}
	turn, err := svc.GetVisibleTurnByMessage(ctx, postgresMessageTestSessionID, assistant.ID)
	if err != nil {
		t.Fatalf("get history turn: %v", err)
	}
	return user, assistant, turn
}

// insertCompactArtifact inserts an ok artifact. The anchor range is what
// assembly reads; the rollup arguments set artifact_level and parent_ids.
func insertCompactArtifact(
	t *testing.T,
	ctx context.Context,
	tx pgx.Tx,
	id string,
	summary string,
	level int,
	anchorStartMs int64,
	anchorEndMs int64,
	parentIDs []string,
	supersededBy *string,
) {
	t.Helper()
	if parentIDs == nil {
		parentIDs = []string{}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO bot_history_message_compacts (
			id, bot_id, session_id, status, summary, artifact_level,
			anchor_start_ms, anchor_end_ms, parent_ids, superseded_by, completed_at
		)
		VALUES ($1, $2, $3, 'ok', $4, $5, $6, $7, $8::uuid[], $9, now())
	`, id, postgresMessageTestBotID, postgresMessageTestSessionID, summary, level,
		anchorStartMs, anchorEndMs, parentIDs, supersededBy); err != nil {
		t.Fatalf("insert compaction artifact %q: %v", summary, err)
	}
}

// linkCompactSources points the given history rows at an artifact.
func linkCompactSources(t *testing.T, ctx context.Context, tx pgx.Tx, artifactID string, messageIDs ...string) {
	t.Helper()
	if _, err := tx.Exec(ctx, `
		UPDATE bot_history_messages
		SET compact_id = $1
		WHERE id = ANY($2::uuid[])
	`, artifactID, messageIDs); err != nil {
		t.Fatalf("link compaction sources: %v", err)
	}
}

func assertArtifactInvalidated(t *testing.T, ctx context.Context, tx pgx.Tx, artifactID string, want bool) {
	t.Helper()
	var invalidated bool
	if err := tx.QueryRow(ctx, `
		SELECT invalidated_at IS NOT NULL
		FROM bot_history_message_compacts
		WHERE id = $1
	`, artifactID).Scan(&invalidated); err != nil {
		t.Fatalf("read artifact %s invalidation: %v", artifactID, err)
	}
	if invalidated != want {
		t.Fatalf("artifact %s invalidated = %v, want %v", artifactID, invalidated, want)
	}
}

// assertLiveSummaries checks the summaries assembly would read, as a set.
func assertLiveSummaries(t *testing.T, ctx context.Context, tx pgx.Tx, sessionID pgtype.UUID, want []string) {
	t.Helper()
	summaries, err := dbsqlc.New(tx).ListActiveCompactionSummariesBySession(ctx, sessionID)
	if err != nil {
		t.Fatalf("list active summaries: %v", err)
	}
	got := make(map[string]int, len(summaries))
	for _, summary := range summaries {
		got[summary.Summary]++
	}
	if len(summaries) != len(want) {
		t.Fatalf("live summaries = %v, want %v", got, want)
	}
	for _, summary := range want {
		if got[summary] != 1 {
			t.Fatalf("live summaries = %v, want %v", got, want)
		}
	}
}

func assertCompactionCandidates(t *testing.T, ctx context.Context, tx pgx.Tx, sessionID pgtype.UUID, want int) {
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

func assertSessionCompactionEpoch(t *testing.T, ctx context.Context, tx pgx.Tx, want int64) {
	t.Helper()
	var got int64
	if err := tx.QueryRow(ctx, `
		SELECT compaction_epoch
		FROM bot_sessions
		WHERE id = $1
	`, postgresMessageTestSessionID).Scan(&got); err != nil {
		t.Fatalf("read session compaction epoch: %v", err)
	}
	if got != want {
		t.Fatalf("session compaction epoch = %d, want %d", got, want)
	}
}

func mustTestUUID(t *testing.T, value string) pgtype.UUID {
	t.Helper()
	parsed, err := dbpkg.ParseUUID(value)
	if err != nil {
		t.Fatalf("parse UUID %q: %v", value, err)
	}
	return parsed
}
