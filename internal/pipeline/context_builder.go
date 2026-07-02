package pipeline

import (
	"context"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
)

// DiscussContextBuilder assembles the discuss LLM context from the RC/TR
// streams. Implemented by contextview.DiscussSDKContextBuilder; injected via
// DiscussDriverDeps to avoid a pipeline -> contextview import cycle.
type DiscussContextBuilder interface {
	BuildDiscussSDKMessages(ctx context.Context, scope contextfrag.Scope, rc RenderedContext, trs []TurnResponseEntry, compactSummary string) ([]sdk.Message, error)
}
