package agentapi

import (
	"context"

	"github.com/felinics/memoh/internal/agent/background"
)

// BackgroundNotifier tells a conversation about the background tasks started
// in it.
type BackgroundNotifier interface {
	Handle(ctx context.Context, evt background.TaskEvent) error
}
