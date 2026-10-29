package builtin

import (
	"context"
	"fmt"
	"testing"
	"time"

	adapters "github.com/felinics/memoh/internal/memory/adapters"
	"github.com/felinics/memoh/internal/memory/migrate"
)

// benchGraphNodes builds the shape a long-lived personal bot accumulates: every
// memory is about the same user, topics repeat, captures spread over a month,
// and some bodies cross-reference other concepts.
func benchGraphNodes(botID string, n int) []migrate.NodeSpec {
	topics := []string{"drinks", "travel", "work", "family", "health", "music", "books", "food", "sport", "code"}
	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	nodes := make([]migrate.NodeSpec, 0, n)
	for i := range n {
		topic := topics[i%len(topics)]
		body := fmt.Sprintf("User note %d about %s: prefers option %d and mentioned project-%d during the weekly review.", i, topic, i%7, i%13)
		if i%5 == 0 && i > 0 {
			body += fmt.Sprintf(" See [[%s]].", topics[(i/5)%len(topics)])
		}
		nodes = append(nodes, migrate.NodeSpec{
			ID:         fmt.Sprintf("%s:mem_%06d", botID, i),
			BotID:      botID,
			Body:       body,
			Hash:       runtimeHash(body),
			Layer:      migrate.LayerNote,
			Subject:    topic,
			Confidence: 0.5,
			ProfileRef: "user:1",
			Topic:      topic,
			CapturedAt: base.Add(time.Duration(i) * 37 * time.Minute),
		})
	}
	return nodes
}

func BenchmarkGraphSearch(b *testing.B) {
	for _, n := range []int{200, 1000} {
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			const botID = "bench-bot"
			store := newFakeWikiStore()
			for _, node := range benchGraphNodes(botID, n) {
				store.nodes[node.ID] = node
			}
			ctx := context.Background()
			rt := NewGraphRuntime(nil, store, nil)
			req := adapters.SearchRequest{BotID: botID, Query: "weekly review about travel", Limit: 8}
			if _, err := rt.Search(ctx, req); err != nil {
				b.Fatalf("warm search: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := rt.Search(ctx, req); err != nil {
					b.Fatalf("search: %v", err)
				}
			}
		})
	}
}
