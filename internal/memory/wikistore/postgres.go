package wikistore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/memory/migrate"
)

// PostgresStore implements Store over the PostgreSQL memory_wiki tables.
type PostgresStore struct {
	q *dbsqlc.Queries
}

// NewPostgres returns a Store backed by the PostgreSQL sqlc Queries.
func NewPostgres(q *dbsqlc.Queries) *PostgresStore {
	return &PostgresStore{q: q}
}

func (s *PostgresStore) UpsertNode(ctx context.Context, node migrate.NodeSpec) (migrate.NodeSpec, error) {
	if s.q == nil {
		return migrate.NodeSpec{}, errors.New("wikistore(postgres): queries not configured")
	}
	r := nodeToRecord(node)
	row, err := s.q.UpsertMemoryNode(ctx, dbsqlc.UpsertMemoryNodeParams{
		ID:               r.ID,
		BotID:            pgUUID(r.BotID),
		Body:             r.Body,
		Hash:             r.Hash,
		Layer:            r.Layer,
		FactType:         r.FactType,
		Subject:          r.Subject,
		Confidence:       r.Confidence,
		Metadata:         marshalJSON(r.Metadata),
		SourceMessageIds: marshalStringList(r.SourceMessageIDs),
		ProfileRef:       r.ProfileRef,
		Topic:            r.Topic,
		CapturedAt:       pgTimestamptz(r.CapturedAt),
		ExpiresAt:        pgTimestamptz(r.ExpiresAt),
	})
	if err != nil {
		return migrate.NodeSpec{}, fmt.Errorf("wikistore(postgres): upsert node: %w", err)
	}
	return recordToNode(pgMemoryNodeToRecord(row)), nil
}

func (s *PostgresStore) GetNode(ctx context.Context, botID, nodeID string) (migrate.NodeSpec, error) {
	if s.q == nil {
		return migrate.NodeSpec{}, errors.New("wikistore(postgres): queries not configured")
	}
	row, err := s.q.GetMemoryNode(ctx, dbsqlc.GetMemoryNodeParams{BotID: pgUUID(botID), ID: nodeID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return migrate.NodeSpec{}, ErrNodeNotFound
		}
		return migrate.NodeSpec{}, fmt.Errorf("wikistore(postgres): get node: %w", err)
	}
	return recordToNode(pgMemoryNodeToRecord(row)), nil
}

func (s *PostgresStore) ListNodes(ctx context.Context, botID string) ([]migrate.NodeSpec, error) {
	if s.q == nil {
		return nil, errors.New("wikistore(postgres): queries not configured")
	}
	rows, err := s.q.ListMemoryNodesByBot(ctx, pgUUID(botID))
	if err != nil {
		return nil, fmt.Errorf("wikistore(postgres): list nodes: %w", err)
	}
	out := make([]migrate.NodeSpec, 0, len(rows))
	for _, r := range rows {
		out = append(out, recordToNode(pgMemoryNodeToRecord(r)))
	}
	return out, nil
}

func (s *PostgresStore) ListNodesByLayer(ctx context.Context, botID string, layer migrate.MemoryLayer) ([]migrate.NodeSpec, error) {
	if s.q == nil {
		return nil, errors.New("wikistore(postgres): queries not configured")
	}
	rows, err := s.q.ListMemoryNodesByBotLayer(ctx, dbsqlc.ListMemoryNodesByBotLayerParams{
		BotID: pgUUID(botID),
		Layer: string(orDefaultLayer(layer)),
	})
	if err != nil {
		return nil, fmt.Errorf("wikistore(postgres): list nodes by layer: %w", err)
	}
	out := make([]migrate.NodeSpec, 0, len(rows))
	for _, r := range rows {
		out = append(out, recordToNode(pgMemoryNodeToRecord(r)))
	}
	return out, nil
}

func (s *PostgresStore) DeleteNode(ctx context.Context, botID, nodeID string) error {
	if s.q == nil {
		return errors.New("wikistore(postgres): queries not configured")
	}
	if err := s.q.DeleteMemoryNode(ctx, dbsqlc.DeleteMemoryNodeParams{BotID: pgUUID(botID), ID: nodeID}); err != nil {
		return fmt.Errorf("wikistore(postgres): delete node: %w", err)
	}
	return nil
}

func (s *PostgresStore) DeleteAllNodes(ctx context.Context, botID string) error {
	if s.q == nil {
		return errors.New("wikistore(postgres): queries not configured")
	}
	if err := s.q.DeleteAllMemoryNodesByBot(ctx, pgUUID(botID)); err != nil {
		return fmt.Errorf("wikistore(postgres): delete all nodes: %w", err)
	}
	return nil
}

func (s *PostgresStore) CountNodes(ctx context.Context, botID string) (int, error) {
	if s.q == nil {
		return 0, errors.New("wikistore(postgres): queries not configured")
	}
	n, err := s.q.CountMemoryNodesByBot(ctx, pgUUID(botID))
	if err != nil {
		return 0, fmt.Errorf("wikistore(postgres): count nodes: %w", err)
	}
	return int(n), nil
}

// ---- Postgres row -> record/spec helpers ----

func pgMemoryNodeToRecord(r dbsqlc.MemoryNode) record {
	rec := record{
		ID:               r.ID,
		BotID:            pgUUIDString(r.BotID),
		Body:             r.Body,
		Hash:             r.Hash,
		Layer:            r.Layer,
		FactType:         r.FactType,
		Subject:          r.Subject,
		Confidence:       r.Confidence,
		Metadata:         unmarshalMetadata(r.Metadata),
		SourceMessageIDs: unmarshalStringList(r.SourceMessageIds),
		ProfileRef:       r.ProfileRef,
		Topic:            r.Topic,
		CapturedAt:       pgTimeValue(r.CapturedAt),
		ExpiresAt:        pgTimeValue(r.ExpiresAt),
	}
	return rec
}

// pgUUID parses a string into a pgtype.UUID.
func pgUUID(s string) pgtype.UUID {
	var u pgtype.UUID
	_ = u.Scan(s)
	return u
}

func pgUUIDString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return u.String()
}

// pgTimestamptz converts a time.Time into a pgtype.Timestamptz; zero time
// produces an invalid (NULL) value.
func pgTimestamptz(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

func pgTimeValue(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time.UTC()
}
