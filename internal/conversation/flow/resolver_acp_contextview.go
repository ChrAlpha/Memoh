package flow

import (
	"context"
	"log/slog"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/contextview"
)

func acpContextViaContextView(ctx context.Context, logger *slog.Logger, contextMarkdown string) (string, string) {
	renderer := &contextview.ACPFullContextRenderer{Config: contextview.ACPRenderConfig{
		Mode:            contextview.ACPRenderModeChat,
		ContextMarkdown: contextMarkdown,
		ContextURI:      acpContextURI,
	}}
	rendered, err := renderer.Render(ctx, contextview.RenderInput{
		Intent: contextfrag.IntentACPRuntimePrompt,
		Target: contextfrag.RenderACPFullContext,
	})
	if err != nil {
		if logger != nil {
			logger.Warn("acp context view render failed; using legacy context", slog.Any("error", err))
		}
		return contextMarkdown, acpContextURI
	}
	payload, ok := rendered.Data.(*contextview.ACPRenderedPayload)
	if !ok {
		if logger != nil {
			logger.Warn("acp context view rendered unexpected payload; using legacy context")
		}
		return contextMarkdown, acpContextURI
	}
	return payload.ContextMarkdown, payload.ContextURI
}
