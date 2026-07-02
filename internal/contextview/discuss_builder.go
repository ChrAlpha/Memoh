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

func (*DiscussSDKContextBuilder) BuildDiscussSDKMessages(ctx context.Context, scope contextfrag.Scope, rc pipeline.RenderedContext, trs []pipeline.TurnResponseEntry, compactSummary string) ([]sdk.Message, error) {
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
				RC:             rc,
				TRs:            trs,
				CompactSummary: compactSummary,
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
