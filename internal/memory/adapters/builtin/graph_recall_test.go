package builtin

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/memory/migrate"
)

// pairwiseExpand is the recall expansion stated over explicit edges: every
// related pair is an undirected edge, a neighbor scores seed × weight × decay,
// and a neighbor's neighbor (other than the seed) that value × decay again.
// botGraph.expand must match it while never materializing the pairs.
func pairwiseExpand(g *botGraph, seeds []graphSeed) map[int]float64 {
	specs := make([]migrate.NodeSpec, len(g.nodes))
	for i, n := range g.nodes {
		specs[i] = n.spec
	}
	type edge struct {
		to     int
		weight float32
	}
	adj := map[int][]edge{}
	for _, e := range migrate.PlanFromNodes(specs) {
		a, b := g.index[e.SrcNode], g.index[e.DstNode]
		adj[a] = append(adj[a], edge{to: b, weight: e.Weight})
		adj[b] = append(adj[b], edge{to: a, weight: e.Weight})
	}
	scores := map[int]float64{}
	for _, s := range seeds {
		scores[s.node] = max(scores[s.node], s.score)
		for _, nb := range adj[s.node] {
			ns := s.score * float64(nb.weight) * recallDecay
			scores[nb.to] = max(scores[nb.to], ns)
			for _, nb2 := range adj[nb.to] {
				if nb2.to == s.node {
					continue
				}
				scores[nb2.to] = max(scores[nb2.to], ns*recallDecay)
			}
		}
	}
	return scores
}

func randomRecallGraph(r *rand.Rand, n int) *botGraph {
	profiles := []string{"", "user:1", "user:1", "user:2"}
	topics := []string{"", "tea", "travel", "work"}
	subjects := []string{"", "tea", "berlin-home", "helix"}
	nodes := make([]migrate.NodeSpec, n)
	for i := range nodes {
		body := fmt.Sprintf("memory %d", i)
		if r.Intn(3) == 0 {
			body += fmt.Sprintf(" see [[%s]]", subjects[1+r.Intn(len(subjects)-1)])
		}
		nodes[i] = migrate.NodeSpec{
			ID:         fmt.Sprintf("bot:mem_%03d", i),
			Body:       body,
			ProfileRef: profiles[r.Intn(len(profiles))],
			Topic:      topics[r.Intn(len(topics))],
			Subject:    subjects[r.Intn(len(subjects))],
			CapturedAt: time.Date(2026, 10, 1+r.Intn(3), r.Intn(24), 0, 0, 0, time.UTC),
		}
	}
	return buildBotGraph(nodes)
}

func TestGraphExpandMatchesPairwiseExpansion(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewSource(7)) //nolint:gosec // a fixed seed keeps the property cases reproducible
	for trial := range 400 {
		g := randomRecallGraph(r, 1+r.Intn(40))
		seeds := make([]graphSeed, 0, 6)
		for range 1 + r.Intn(6) {
			// Semantic seeds can score at or below zero.
			seeds = append(seeds, graphSeed{node: r.Intn(len(g.nodes)), score: r.Float64()*1.2 - 0.2})
		}
		hits, err := g.expand(context.Background(), seeds)
		if err != nil {
			t.Fatalf("trial %d: expand: %v", trial, err)
		}
		got := map[int]float64{}
		for _, hit := range hits {
			got[hit.node] = hit.score
		}
		want := pairwiseExpand(g, seeds)
		if len(got) != len(want) {
			t.Fatalf("trial %d: reached %d nodes, want %d\ngot  %v\nwant %v", trial, len(got), len(want), got, want)
		}
		for node, score := range want {
			if got[node] != score {
				t.Fatalf("trial %d: node %s score = %v, want %v", trial, g.nodes[node].spec.ID, got[node], score)
			}
		}
	}
}

func TestGraphRecallRanksTiesByRecency(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	g := buildBotGraph([]migrate.NodeSpec{
		{ID: "b", Body: "older", CapturedAt: day},
		{ID: "a", Body: "newer", CapturedAt: day.Add(time.Hour)},
		{ID: "c", Body: "same time as a", CapturedAt: day.Add(time.Hour)},
	})
	hits := g.top([]graphHit{{node: 0, score: 0.5}, {node: 2, score: 0.5}, {node: 1, score: 0.5}}, 3)
	var order []string
	for _, hit := range hits {
		order = append(order, g.nodes[hit.node].spec.ID)
	}
	if fmt.Sprint(order) != "[a c b]" {
		t.Fatalf("tie order = %v, want [a c b] (newest first, then by ID)", order)
	}
}

func TestGraphExpandStopsWhenContextEnds(t *testing.T) {
	t.Parallel()
	g := randomRecallGraph(rand.New(rand.NewSource(1)), 20) //nolint:gosec // fixed seed fixture
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.expand(ctx, []graphSeed{{node: 0, score: 1}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expand error = %v, want context.Canceled", err)
	}
}
