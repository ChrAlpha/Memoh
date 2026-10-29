package builtin

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/felinics/memoh/internal/memory/migrate"
	"github.com/felinics/memoh/internal/memory/wikistore"
)

// graphCacheTTL is how long a cached bot graph stays fresh before it is rebuilt
// from the store. Writes invalidate the entry immediately, so this TTL only
// bounds staleness when the same process keeps serving reads without writes
// (e.g. a long-idle bot).
const graphCacheTTL = 5 * time.Minute

// graphNode is a cached node with its body lower-cased for lexical scoring.
type graphNode struct {
	spec      migrate.NodeSpec
	lowerBody string
}

// graphLink is one end of a cross-reference, seen from the other end.
type graphLink struct {
	to     int
	weight float32
}

// botGraph is the cached graph for a single bot: its nodes and the relations
// derived from them (migrate.DeriveRelations). Relations are kept as groups,
// so a profile shared by every node costs one group, not a pair per node.
type botGraph struct {
	nodes  []graphNode
	index  map[string]int
	groups []migrate.RelationGroup
	// memberOf lists the groups each node belongs to.
	memberOf [][]int
	// refs lists each node's cross-references in both directions.
	refs      [][]graphLink
	edgeCount int
	builtAt   time.Time
}

// graphCache caches per-bot in-memory graphs, rebuilt lazily from the
// WikiStore. It is safe for concurrent use. A write to the store should call
// invalidate(botID) so the next read rebuilds.
type graphCache struct {
	mu       sync.RWMutex
	graphs   map[string]*botGraph
	versions map[string]uint64
}

func newGraphCache() *graphCache {
	return &graphCache{
		graphs:   make(map[string]*botGraph),
		versions: make(map[string]uint64),
	}
}

// getOrBuild returns the cached graph for botID, rebuilding it from store if
// missing or stale (older than graphCacheTTL).
func (c *graphCache) getOrBuild(ctx context.Context, botID string, store wikistore.Store) (*botGraph, error) {
	for {
		c.mu.RLock()
		g := c.graphs[botID]
		version := c.versions[botID]
		c.mu.RUnlock()
		if g != nil && time.Since(g.builtAt) < graphCacheTTL {
			return g, nil
		}

		nodes, err := store.ListNodes(ctx, botID)
		if err != nil {
			return nil, fmt.Errorf("graph cache: list nodes: %w", err)
		}
		built := buildBotGraph(nodes)

		c.mu.Lock()
		if c.versions[botID] != version {
			c.mu.Unlock()
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			continue
		}
		if current := c.graphs[botID]; current != nil && time.Since(current.builtAt) < graphCacheTTL {
			c.mu.Unlock()
			return current, nil
		}
		c.graphs[botID] = built
		c.mu.Unlock()
		return built, nil
	}
}

// invalidate drops the cached graph for botID so the next read rebuilds.
func (c *graphCache) invalidate(botID string) {
	c.mu.Lock()
	delete(c.graphs, botID)
	c.versions[botID]++
	c.mu.Unlock()
}

func (c *graphCache) version(botID string) string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	version := c.versions[botID]
	c.mu.RUnlock()
	return strconv.FormatUint(version, 10)
}

// buildBotGraph indexes nodes and derives their relations. A repeated node ID
// keeps its first occurrence.
func buildBotGraph(nodes []migrate.NodeSpec) *botGraph {
	g := &botGraph{
		nodes: make([]graphNode, 0, len(nodes)),
		index: make(map[string]int, len(nodes)),
	}
	specs := make([]migrate.NodeSpec, 0, len(nodes))
	for _, n := range nodes {
		if _, dup := g.index[n.ID]; dup {
			continue
		}
		g.index[n.ID] = len(g.nodes)
		g.nodes = append(g.nodes, graphNode{spec: n, lowerBody: strings.ToLower(n.Body)})
		specs = append(specs, n)
	}
	rel := migrate.DeriveRelations(specs)
	g.groups = rel.Groups
	g.memberOf = make([][]int, len(g.nodes))
	for gi, group := range rel.Groups {
		for _, member := range group.Members {
			g.memberOf[member] = append(g.memberOf[member], gi)
		}
	}
	g.refs = make([][]graphLink, len(g.nodes))
	for _, ref := range rel.Refs {
		g.refs[ref.Src] = append(g.refs[ref.Src], graphLink{to: ref.Dst, weight: ref.Weight})
		g.refs[ref.Dst] = append(g.refs[ref.Dst], graphLink{to: ref.Src, weight: ref.Weight})
	}
	g.edgeCount = rel.EdgeCount()
	g.builtAt = time.Now()
	return g
}
