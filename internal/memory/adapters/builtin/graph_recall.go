package builtin

import (
	"context"
	"sort"

	memseg "github.com/felinics/memoh/internal/memory/segment"
)

// recallDecay discounts each relation hop away from a seed.
const recallDecay = 0.6

// ctxCheckInterval is how many nodes a recall loop scores between context
// checks.
const ctxCheckInterval = 256

// graphSeed is a node recall starts from, with its match score.
type graphSeed struct {
	node  int
	score float64
}

// graphHit is a recalled node with its score.
type graphHit struct {
	node  int
	score float64
}

// seeds scores every node against query and returns the best limit nodes to
// expand from. semantic carries pgvector scores by node ID; a node keeps the
// better of its semantic and lexical score. With a non-empty query, a node
// without a lexical match seeds only through its semantic score.
func (g *botGraph) seeds(ctx context.Context, query memseg.Query, emptyQuery bool, semantic map[string]float64, limit int) ([]graphSeed, error) {
	scores := make(map[int]float64, len(semantic))
	for id, score := range semantic {
		if i, ok := g.index[id]; ok {
			scores[i] = max(scores[i], score)
		}
	}
	for i, node := range g.nodes {
		if i%ctxCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		s := query.ScoreLower(node.lowerBody)
		if s <= 0 && !emptyQuery {
			continue
		}
		scores[i] = max(scores[i], s)
	}
	seeds := make([]graphSeed, 0, len(scores))
	for i, score := range scores {
		seeds = append(seeds, graphSeed{node: i, score: score})
	}
	sort.Slice(seeds, func(a, b int) bool {
		return g.ranksBefore(seeds[a].node, seeds[a].score, seeds[b].node, seeds[b].score)
	})
	if len(seeds) > limit {
		seeds = seeds[:limit]
	}
	return seeds, nil
}

// expand scores every node within two relation hops of a seed. A seed keeps
// its own score, a neighbor gets seed × weight × decay, and a neighbor's
// neighbor seed × weight × decay², where weight is the strongest relation
// between the seed and the first-hop neighbor. Each node takes its best path,
// and no score drops below zero.
//
// A group relates all of its members, so a hop into a group raises a floor
// shared by its members instead of visiting every pair: the work per seed is
// linear in the size of the seed's groups, not quadratic.
func (g *botGraph) expand(ctx context.Context, seeds []graphSeed) ([]graphHit, error) {
	n := len(g.nodes)
	point := make([]float64, n)
	reached := make([]bool, n)
	floor := make([]float64, len(g.groups))
	opened := make([]bool, len(g.groups))

	// Per-seed scratch, stamped with the seed ordinal instead of cleared.
	hop1 := make([]float64, n)
	hop1Stamp := make([]int, n)
	hop2 := make([]float64, len(g.groups))
	hop2Stamp := make([]int, len(g.groups))
	var neighbors, hop2Groups []int

	raise := func(i int, v float64) {
		reached[i] = true
		point[i] = max(point[i], v)
	}
	open := func(gi int, v float64) {
		opened[gi] = true
		floor[gi] = max(floor[gi], v)
	}

	for ord, seed := range seeds {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		mark := ord + 1
		neighbors = neighbors[:0]
		reach := func(i int, weight float64) {
			if i == seed.node {
				return
			}
			if hop1Stamp[i] != mark {
				hop1Stamp[i] = mark
				hop1[i] = weight
				neighbors = append(neighbors, i)
				return
			}
			hop1[i] = max(hop1[i], weight)
		}

		raise(seed.node, seed.score)
		for _, gi := range g.memberOf[seed.node] {
			group := g.groups[gi]
			weight := float64(group.Weight)
			open(gi, seed.score*weight*recallDecay)
			for _, member := range group.Members {
				reach(member, weight)
			}
		}
		for _, link := range g.refs[seed.node] {
			weight := float64(link.weight)
			raise(link.to, seed.score*weight*recallDecay)
			reach(link.to, weight)
		}

		// Second hop. A neighbor opens every group it belongs to; each group
		// takes the strongest neighbor that opens it.
		hop2Groups = hop2Groups[:0]
		for _, nb := range neighbors {
			v := seed.score * hop1[nb] * recallDecay * recallDecay
			for _, gi := range g.memberOf[nb] {
				if hop2Stamp[gi] != mark {
					hop2Stamp[gi] = mark
					hop2[gi] = v
					hop2Groups = append(hop2Groups, gi)
					continue
				}
				hop2[gi] = max(hop2[gi], v)
			}
			for _, link := range g.refs[nb] {
				raise(link.to, v)
			}
		}
		for _, gi := range hop2Groups {
			open(gi, hop2[gi])
		}
	}

	hits := make([]graphHit, 0, n)
	for i := range g.nodes {
		if i%ctxCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		hit, score := reached[i], point[i]
		for _, gi := range g.memberOf[i] {
			if opened[gi] {
				hit = true
				score = max(score, floor[gi])
			}
		}
		if hit {
			hits = append(hits, graphHit{node: i, score: score})
		}
	}
	return hits, nil
}

// ranksBefore orders recall candidates by score, then by the most recent
// capture, then by node ID.
func (g *botGraph) ranksBefore(a int, aScore float64, b int, bScore float64) bool {
	if aScore != bScore {
		return aScore > bScore
	}
	at, bt := g.nodes[a].spec.CapturedAt, g.nodes[b].spec.CapturedAt
	if !at.Equal(bt) {
		return at.After(bt)
	}
	return g.nodes[a].spec.ID < g.nodes[b].spec.ID
}

// top returns the best limit hits in rank order.
func (g *botGraph) top(hits []graphHit, limit int) []graphHit {
	sort.Slice(hits, func(a, b int) bool {
		return g.ranksBefore(hits[a].node, hits[a].score, hits[b].node, hits[b].score)
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}
