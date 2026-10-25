package checkpoint

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// movingQueries serves a session whose active checkpoint another writer
// moves while Active repairs it: each read returns the next checkpoint of
// active, and a repair succeeds only once moves runs out.
type movingQueries struct {
	dbstore.Queries
	active  []sqlc.BotHistoryCheckpoint
	broken  map[pgtype.UUID]bool
	moves   int
	reads   int
	repairs []sqlc.SetHistoryCheckpointParams
}

func (q *movingQueries) GetActiveHistoryCheckpoint(context.Context, pgtype.UUID) (sqlc.BotHistoryCheckpoint, error) {
	if len(q.active) == 0 {
		return sqlc.BotHistoryCheckpoint{}, pgx.ErrNoRows
	}
	row := q.active[min(q.reads, len(q.active)-1)]
	q.reads++
	return row, nil
}

func (q *movingQueries) CheckHistoryCheckpoint(_ context.Context, id pgtype.UUID) (sqlc.CheckHistoryCheckpointRow, error) {
	if q.broken[id] {
		return sqlc.CheckHistoryCheckpointRow{MissingRows: 1}, nil
	}
	return sqlc.CheckHistoryCheckpointRow{}, nil
}

func (q *movingQueries) ListHistoryCheckpointChain(_ context.Context, id pgtype.UUID) ([]sqlc.BotHistoryCheckpoint, error) {
	for _, row := range q.active {
		if row.ID == id {
			return []sqlc.BotHistoryCheckpoint{row}, nil
		}
	}
	return nil, nil
}

func (q *movingQueries) SetHistoryCheckpoint(_ context.Context, arg sqlc.SetHistoryCheckpointParams) (int64, error) {
	q.repairs = append(q.repairs, arg)
	if q.moves > 0 {
		q.moves--
		return 0, nil
	}
	return 1, nil
}

func checkpointRow(id string) sqlc.BotHistoryCheckpoint {
	return sqlc.BotHistoryCheckpoint{
		ID:        dbpkg.ParseUUIDOrEmpty(id),
		SessionID: dbpkg.ParseUUIDOrEmpty(testSessionID),
		Items:     []byte(`[]`),
	}
}

// A repair moves the session only from the checkpoint it read; when another
// writer moved it first, Active reads the session again and returns what it
// holds now.
func TestActiveReadsAgainWhenTheSessionMovedDuringARepair(t *testing.T) {
	broken, moved := checkpointRow(testCheckpointID), checkpointRow(testSummaryID)
	queries := &movingQueries{
		active: []sqlc.BotHistoryCheckpoint{broken, moved},
		broken: map[pgtype.UUID]bool{broken.ID: true},
		moves:  1,
	}
	got, err := NewStore(nil, queries).Active(context.Background(), testSessionID)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if got.ID != testSummaryID {
		t.Fatalf("Active = %q, want the checkpoint the other writer made active", got.ID)
	}
	if len(queries.repairs) != 1 || queries.repairs[0].ExpectedCheckpointID != broken.ID || queries.repairs[0].CheckpointID.Valid {
		t.Fatalf("repairs = %+v, want one from the broken checkpoint to none", queries.repairs)
	}
}

func TestActiveGivesUpOnASessionThatKeepsMoving(t *testing.T) {
	broken := checkpointRow(testCheckpointID)
	queries := &movingQueries{
		active: []sqlc.BotHistoryCheckpoint{broken},
		broken: map[pgtype.UUID]bool{broken.ID: true},
		moves:  activeReads,
	}
	_, err := NewStore(nil, queries).Active(context.Background(), testSessionID)
	if err == nil || !strings.Contains(err.Error(), "kept moving") {
		t.Fatalf("Active = %v, want an error after %d reads", err, activeReads)
	}
	if queries.reads != activeReads {
		t.Fatalf("reads = %d, want %d", queries.reads, activeReads)
	}
}

func TestActiveOfASessionWithoutACheckpointIsZero(t *testing.T) {
	got, err := NewStore(nil, &movingQueries{}).Active(context.Background(), testSessionID)
	if err != nil || got.ID != "" || len(got.Items) != 0 {
		t.Fatalf("Active = %+v, %v, want the zero checkpoint", got, err)
	}
}
