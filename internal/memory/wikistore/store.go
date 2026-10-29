// Package wikistore provides the store over the memory_nodes table. It defines
// the Store interface in terms of plain Go POJOs (migrate.NodeSpec) so that the
// graph runtime never touches sqlc or driver-specific types. Relations between
// nodes are derived from the nodes themselves (migrate.DeriveRelations), so
// the store keeps no edges. The PostgreSQL implementation wraps the
// sqlc-generated queries.
package wikistore

import (
	"context"
	"time"

	"github.com/felinics/memoh/internal/memory/migrate"
)

// Store is the backend-agnostic contract over the memory wiki nodes. All
// methods are scoped to a single bot_id. NodeSpec comes from the migrate
// package and carries no driver-specific types.
type Store interface {
	// UpsertNode inserts or updates a single node by id (idempotent on conflict).
	UpsertNode(ctx context.Context, node migrate.NodeSpec) (migrate.NodeSpec, error)
	// GetNode returns one node by id, or ErrNodeNotFound.
	GetNode(ctx context.Context, botID, nodeID string) (migrate.NodeSpec, error)
	// ListNodes returns every node for a bot in captured_at ascending order.
	ListNodes(ctx context.Context, botID string) ([]migrate.NodeSpec, error)
	// ListNodesByLayer returns nodes for a bot filtered by layer.
	ListNodesByLayer(ctx context.Context, botID string, layer migrate.MemoryLayer) ([]migrate.NodeSpec, error)
	// DeleteNode removes a node.
	DeleteNode(ctx context.Context, botID, nodeID string) error
	// DeleteAllNodes removes every node for a bot.
	DeleteAllNodes(ctx context.Context, botID string) error
	// CountNodes returns the node count for a bot.
	CountNodes(ctx context.Context, botID string) (int, error)
}

// ErrNodeNotFound is returned by GetNode when no node matches the id.
var ErrNodeNotFound = errNodeNotFound{}

type errNodeNotFound struct{}

func (errNodeNotFound) Error() string { return "wikistore: node not found" }

// Is lets errors.Is match this sentinel across the two backends (which may
// wrap it differently).
func (errNodeNotFound) Is(target error) bool {
	_, ok := target.(errNodeNotFound)
	return ok
}

// record is the canonical struct returned by Read operations; it mirrors
// migrate.NodeSpec but is internal so callers depend on migrate.* types only.
type record struct {
	ID               string
	BotID            string
	Body             string
	Hash             string
	Layer            string
	FactType         string
	Subject          string
	Confidence       float32
	Metadata         map[string]any
	SourceMessageIDs []string
	ProfileRef       string
	Topic            string
	CapturedAt       time.Time
	ExpiresAt        time.Time
}
