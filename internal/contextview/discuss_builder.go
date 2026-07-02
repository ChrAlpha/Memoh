package contextview

import (
	"context"
	"fmt"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/pipeline"
)

// DiscussSDKContextBuilder implements pipeline.DiscussContextBuilder by
// running the discuss collector through the context view pipeline.
type DiscussSDKContextBuilder struct{}

func (*DiscussSDKContextBuilder) BuildDiscussSDKMessages(ctx context.Context, scope contextfrag.Scope, input pipeline.DiscussContextInput) ([]sdk.Message, error) {
	builder := NewBuilder(
		NewMapCollectorRegistry(&DiscussContextCollector{}),
		&FragmentSelector{},
		IdentityPlacer{},
		NewMapRendererRegistry(&SDKMessagesRenderer{}),
	)
	view, err := builder.Build(ctx, BuildInput{
		Scope:  scope,
		Intent: contextfrag.IntentDiscussReply,
		Sources: []SourceSpec{{
			Name: "discuss_context",
			Config: DiscussContextConfig{
				RC:             input.RC,
				TRs:            input.TRs,
				CompactSummary: input.CompactSummary,
				LateBinding:    input.LateBinding,
				InlineImages:   input.InlineImages,
			},
		}},
		Targets: []contextfrag.RenderTarget{contextfrag.RenderSDKMessages},
	})
	if err != nil {
		return nil, err
	}
	payload, ok := view.Rendered[contextfrag.RenderSDKMessages].Data.(*SDKRenderedPayload)
	if !ok {
		return nil, fmt.Errorf("unexpected discuss payload type %T", view.Rendered[contextfrag.RenderSDKMessages].Data)
	}
	return payload.Messages, nil
}

func (*DiscussSDKContextBuilder) BuildDiscussACPPrompt(ctx context.Context, messages []pipeline.ContextMessage, lateBinding string) (string, error) {
	renderer := &ACPFullContextRenderer{Config: ACPRenderConfig{
		Mode:               ACPRenderModeDiscuss,
		DiscussMessages:    messages,
		DiscussLateBinding: lateBinding,
	}}
	rendered, err := renderer.Render(ctx, RenderInput{
		Intent: contextfrag.IntentACPRuntimePrompt,
		Target: contextfrag.RenderACPFullContext,
	})
	if err != nil {
		return "", err
	}
	payload, ok := rendered.Data.(*ACPRenderedPayload)
	if !ok {
		return "", fmt.Errorf("unexpected acp payload type %T", rendered.Data)
	}
	return payload.ContextMarkdown, nil
}
