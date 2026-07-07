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

// DiscussContextInput carries every discuss source for one turn: the RC/TR
// streams plus the late-binding instruction and freshly surfaced inline
// images that previously were appended after context assembly. SystemFrags
// carries the typed system prompt fragments already built by the resolver;
// when present the builder uses them instead of reverse-parsing the flat
// system string.
type DiscussContextInput struct {
	RC             RenderedContext
	TRs            []TurnResponseEntry
	CompactSummary string
	LateBinding    string
	InlineImages   []sdk.ImagePart
	SystemFrags    []contextfrag.ContextFrag
}
