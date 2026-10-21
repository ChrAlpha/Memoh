package message

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// turnWriteQueries is a store without atomic direct writes, so rows take the
// create-then-file path. Its request lookups find nothing, and it counts the
// writes that would guess a turn.
type turnWriteQueries struct {
	dbstore.Queries

	created      int
	deleted      []pgtype.UUID
	latestAppend int
	turnsCreated int
}

func (*turnWriteQueries) GetSessionByID(_ context.Context, id pgtype.UUID) (sqlc.BotSession, error) {
	return sqlc.BotSession{ID: id, Type: "chat", SessionMode: "chat", RuntimeType: "model"}, nil
}

func (q *turnWriteQueries) CreateMessage(_ context.Context, arg sqlc.CreateMessageParams) (sqlc.CreateMessageRow, error) {
	q.created++
	return sqlc.CreateMessageRow{
		ID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, BotID: arg.BotID, SessionID: arg.SessionID,
		Role: arg.Role, Content: arg.Content, Metadata: arg.Metadata, Usage: arg.Usage,
		SessionMode: arg.SessionMode, RuntimeType: arg.RuntimeType, DisplayText: arg.DisplayText,
		CreatedAt: pgtype.Timestamptz{Valid: true},
	}, nil
}

func (q *turnWriteQueries) CreateHistoryTurn(context.Context, sqlc.CreateHistoryTurnParams) (dbstore.HistoryTurn, error) {
	q.turnsCreated++
	return dbstore.HistoryTurn{}, nil
}

func (*turnWriteQueries) AppendMessageToHistoryTurnByRequest(context.Context, sqlc.AppendMessageToHistoryTurnByRequestParams) (pgtype.UUID, error) {
	return pgtype.UUID{}, pgx.ErrNoRows
}

func (*turnWriteQueries) BindHistoryTurnAssistantByRequest(context.Context, sqlc.BindHistoryTurnAssistantByRequestParams) (dbstore.HistoryTurn, error) {
	return dbstore.HistoryTurn{}, pgx.ErrNoRows
}

func (*turnWriteQueries) BindLatestHistoryTurnAssistant(context.Context, sqlc.BindLatestHistoryTurnAssistantParams) (dbstore.HistoryTurn, error) {
	return dbstore.HistoryTurn{}, pgx.ErrNoRows
}

func (*turnWriteQueries) GetLatestVisibleHistoryTurnBySession(context.Context, pgtype.UUID) (dbstore.HistoryTurn, error) {
	return dbstore.HistoryTurn{}, pgx.ErrNoRows
}

func (*turnWriteQueries) LinkMessageToHistoryTurn(_ context.Context, arg sqlc.LinkMessageToHistoryTurnParams) (pgtype.UUID, error) {
	return arg.MessageID, nil
}

func (q *turnWriteQueries) AppendMessageToLatestHistoryTurn(_ context.Context, arg sqlc.AppendMessageToLatestHistoryTurnParams) (pgtype.UUID, error) {
	q.latestAppend++
	return arg.MessageID, nil
}

func (*turnWriteQueries) LinkUnassignedMessagesAfterHistoryTurnAssistant(context.Context, pgtype.UUID) error {
	return nil
}

func (q *turnWriteQueries) DeleteMessagesByIDs(_ context.Context, ids []pgtype.UUID) error {
	q.deleted = append(q.deleted, ids...)
	return nil
}

// A run's assistant or tool row is filed under its request row's turn. When it
// names no request row, or the row it names is gone, the store reports it
// instead of putting the row in whichever turn is latest.
func TestRunRowWithoutResolvableRequestIsRejected(t *testing.T) {
	const (
		botID     = "11111111-1111-1111-1111-111111111111"
		sessionID = "22222222-2222-2222-2222-222222222222"
	)
	for _, role := range []string{"assistant", "tool"} {
		for _, tc := range []struct {
			name        string
			requestID   string
			wantCreated int
		}{
			{name: "no request row", wantCreated: 0},
			{name: "request row gone", requestID: "33333333-3333-3333-3333-333333333333", wantCreated: 1},
		} {
			t.Run(role+"/"+tc.name, func(t *testing.T) {
				queries := &turnWriteQueries{}
				svc := NewService(nil, queries)
				_, err := svc.Persist(context.Background(), PersistInput{
					BotID: botID, SessionID: sessionID, Role: role, RunID: uuid.NewString(),
					TurnRequestMessageID: tc.requestID, Content: []byte(`{"type":"text","text":"x"}`),
				})
				if !errors.Is(err, ErrRunRowTurnNotFound) {
					t.Fatalf("Persist() error = %v, want ErrRunRowTurnNotFound", err)
				}
				if queries.latestAppend != 0 || queries.turnsCreated != 0 {
					t.Fatalf("guessed a turn: latest appends %d, turns created %d", queries.latestAppend, queries.turnsCreated)
				}
				if queries.created != tc.wantCreated || len(queries.deleted) != tc.wantCreated {
					t.Fatalf("rows created/cleaned = %d/%d, want %d/%d", queries.created, len(queries.deleted), tc.wantCreated, tc.wantCreated)
				}
			})
		}
	}
}

// A row that belongs to no run (a background task notice) names no request
// row by design and goes to the latest visible turn.
func TestRowOfNoRunAppendsToTheLatestTurn(t *testing.T) {
	for _, role := range []string{"assistant", "tool"} {
		t.Run(role, func(t *testing.T) {
			queries := &turnWriteQueries{}
			svc := NewService(nil, queries)
			_, err := svc.Persist(context.Background(), PersistInput{
				BotID: "11111111-1111-1111-1111-111111111111", SessionID: "22222222-2222-2222-2222-222222222222",
				Role: role, Content: []byte(`{"type":"text","text":"x"}`),
			})
			if err != nil {
				t.Fatalf("Persist() error = %v", err)
			}
			if queries.latestAppend != 1 {
				t.Fatalf("latest turn appends = %d, want 1", queries.latestAppend)
			}
		})
	}
}
