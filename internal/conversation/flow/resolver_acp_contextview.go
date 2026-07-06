package flow

import (
	"context"
	"log/slog"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/contextview"
)

// acpContextViaContextView builds the ACP chat context view. The current user
// query joins the build as a KindCurrentUserMessage fragment so the manifest
// records the full request, but the chat renderer keeps it out of the context
// document: the returned prompt is the fragment's text (the query verbatim),
// delivered to the ACP runtime as the prompt itself.
func acpContextViaContextView(ctx context.Context, logger *slog.Logger, sections []contextview.ACPSection, query string) (string, string, string) {
	builder := contextview.NewBuilder(
		contextview.NewMapCollectorRegistry(&contextview.ACPSectionsCollector{}, &contextview.CurrentUserCollector{}),
		&contextview.FragmentSelector{},
		contextview.StablePrefixPlacer{},
		contextview.NewMapRendererRegistry(&contextview.ACPFullContextRenderer{Config: contextview.ACPRenderConfig{
			Mode:       contextview.ACPRenderModeChat,
			ContextURI: acpContextURI,
		}}),
	)
	view, err := builder.Build(ctx, contextview.BuildInput{
		Intent: contextfrag.IntentACPRuntimePrompt,
		Sources: []contextview.SourceSpec{
			{Name: "acp_sections", Config: contextview.ACPSectionsConfig{Sections: sections}},
			{Name: "current_user", Config: contextview.CurrentUserConfig{Query: query}},
		},
		Targets: []contextfrag.RenderTarget{contextfrag.RenderACPFullContext},
	})
	if err != nil {
		if logger != nil {
			logger.Error("acp context view build failed; assembling sections directly", slog.Any("error", err))
		}
		return finalizeACPSections(sections), acpContextURI, query
	}
	rendered := view.Rendered[contextfrag.RenderACPFullContext]
	payload, ok := rendered.Data.(*contextview.ACPRenderedPayload)
	if !ok {
		if logger != nil {
			logger.Error("acp context view rendered unexpected payload; assembling sections directly")
		}
		return finalizeACPSections(sections), acpContextURI, query
	}
	return payload.ContextMarkdown, payload.ContextURI, acpCurrentUserPrompt(view.Selected, query)
}

func acpCurrentUserPrompt(selected []contextfrag.ContextFrag, fallback string) string {
	for _, frag := range selected {
		if frag.Kind != contextfrag.KindCurrentUserMessage {
			continue
		}
		for _, part := range frag.Parts {
			if part.Type == contextfrag.PartText && part.Text != "" {
				return part.Text
			}
		}
	}
	return fallback
}

func finalizeACPSections(sections []contextview.ACPSection) string {
	blocks := make([]string, 0, len(sections))
	for _, section := range sections {
		blocks = append(blocks, section.Text)
	}
	return contextview.FinalizeACPContextMarkdown(blocks)
}
