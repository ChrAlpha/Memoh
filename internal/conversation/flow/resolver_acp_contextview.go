package flow

import (
	"context"
	"log/slog"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/contextview"
)

func acpContextViaContextView(ctx context.Context, logger *slog.Logger, sections []contextview.ACPSection) (string, string) {
	builder := contextview.NewBuilder(
		contextview.NewMapCollectorRegistry(&contextview.ACPSectionsCollector{}),
		&contextview.FragmentSelector{},
		contextview.StablePrefixPlacer{},
		contextview.NewMapRendererRegistry(&contextview.ACPFullContextRenderer{Config: contextview.ACPRenderConfig{
			Mode:       contextview.ACPRenderModeChat,
			ContextURI: acpContextURI,
		}}),
	)
	view, err := builder.Build(ctx, contextview.BuildInput{
		Intent: contextfrag.IntentACPRuntimePrompt,
		Sources: []contextview.SourceSpec{{
			Name:   "acp_sections",
			Config: contextview.ACPSectionsConfig{Sections: sections},
		}},
		Targets: []contextfrag.RenderTarget{contextfrag.RenderACPFullContext},
	})
	if err != nil {
		if logger != nil {
			logger.Error("acp context view build failed; assembling sections directly", slog.Any("error", err))
		}
		return finalizeACPSections(sections), acpContextURI
	}
	rendered := view.Rendered[contextfrag.RenderACPFullContext]
	payload, ok := rendered.Data.(*contextview.ACPRenderedPayload)
	if !ok {
		if logger != nil {
			logger.Error("acp context view rendered unexpected payload; assembling sections directly")
		}
		return finalizeACPSections(sections), acpContextURI
	}
	return payload.ContextMarkdown, payload.ContextURI
}

func finalizeACPSections(sections []contextview.ACPSection) string {
	blocks := make([]string, 0, len(sections))
	for _, section := range sections {
		blocks = append(blocks, section.Text)
	}
	return contextview.FinalizeACPContextMarkdown(blocks)
}
