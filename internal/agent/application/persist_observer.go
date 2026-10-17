package application

import (
	"context"

	"github.com/felinics/memoh/internal/agent/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// persistObserver hands what the history writer made durable to the memory and
// compaction policies, which live above it.
type persistObserver struct{ s *Service }

func (o persistObserver) RoundPersisted(ctx context.Context, req runreq.ChatRequest, persisted []messagepkg.Message) {
	o.s.storeMemory(ctx, req, persisted)
}

func (o persistObserver) RunCommitted(ctx context.Context, req runreq.ChatRequest, rc runreq.ResolvedContext, inputTokens int) {
	o.s.maybeCompact(ctx, req, rc, inputTokens)
}
