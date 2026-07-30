package contextview

import (
	"context"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/chat/timeline"
)

// DiscussContextBuilder assembles the discuss LLM context from the RC/TR
// streams. Implemented by contextview.DiscussSDKContextBuilder; injected via
// DiscussDriverDeps to avoid a pipeline -> contextview import cycle.
type DiscussContextBuilder interface {
	BuildDiscussSDKMessages(ctx context.Context, scope contextfrag.Scope, input DiscussContextInput) ([]sdk.Message, error)
	BuildDiscussACPPrompt(ctx context.Context, scope contextfrag.Scope, input DiscussContextInput) (string, error)
	// BuildDiscussACPPromptWithLifecycle mirrors BuildDiscussACPPrompt but
	// also returns the context view manifest, so the caller can record a
	// context lifecycle snapshot for the discuss-ACP build.
	BuildDiscussACPPromptWithLifecycle(ctx context.Context, scope contextfrag.Scope, input DiscussContextInput) (string, *contextfrag.Manifest, error)
	// CollectDiscussSourceFrags returns the discuss turn as first-class
	// source fragments (system prompt plus the discuss stream) for the
	// fragments-first provider run config.
	CollectDiscussSourceFrags(ctx context.Context, scope contextfrag.Scope, system string, input DiscussContextInput) ([]contextfrag.ContextFrag, error)
}

// DiscussContextInput carries every discuss source for one turn. When
// ComposedMessages is non-nil, it is the authoritative timeline composition;
// otherwise the collector composes the legacy RC/TR inputs. SystemFrags
// carries the typed system prompt fragments already built by the resolver.
type DiscussContextInput struct {
	ComposedMessages []timeline.ContextMessage
	RC               timeline.RenderedContext
	TRs              []timeline.TurnResponseEntry
	CompactSummary   string
	LateBinding      string
	InlineImages     []sdk.ImagePart
	SystemFrags      []contextfrag.ContextFrag
}
