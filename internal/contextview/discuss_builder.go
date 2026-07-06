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

// CollectDiscussSourceFrags returns the discuss turn as first-class source
// fragments: the resolved system prompt plus the discuss stream, ready to be
// carried on the run config for the fragments-first provider view. The typed
// system fragments supplied on the input are authoritative; the flat system
// string is only reverse-parsed when the caller provided none.
func (*DiscussSDKContextBuilder) CollectDiscussSourceFrags(ctx context.Context, scope contextfrag.Scope, system string, input pipeline.DiscussContextInput) ([]contextfrag.ContextFrag, error) {
	systemFrags := input.SystemFrags
	if len(systemFrags) == 0 {
		collected, err := (&SystemPromptCollector{}).Collect(ctx, CollectRequest{
			Scope:  scope,
			Intent: contextfrag.IntentRunConfigPreProvider,
			Config: SystemPromptConfig{System: system, SplitWorkspace: true},
		})
		if err != nil {
			return nil, err
		}
		systemFrags = collected
	}
	discussFrags, err := (&DiscussContextCollector{}).Collect(ctx, CollectRequest{
		Scope:  scope,
		Intent: contextfrag.IntentRunConfigPreProvider,
		Config: DiscussContextConfig{
			RC:             input.RC,
			TRs:            input.TRs,
			CompactSummary: input.CompactSummary,
			LateBinding:    input.LateBinding,
			InlineImages:   input.InlineImages,
		},
	})
	if err != nil {
		return nil, err
	}
	return append(systemFrags, discussFrags...), nil
}

func (*DiscussSDKContextBuilder) BuildDiscussACPPrompt(ctx context.Context, scope contextfrag.Scope, input pipeline.DiscussContextInput) (string, error) {
	builder := NewBuilder(
		NewMapCollectorRegistry(&DiscussContextCollector{}),
		&FragmentSelector{},
		IdentityPlacer{},
		NewMapRendererRegistry(&ACPFullContextRenderer{Config: ACPRenderConfig{Mode: ACPRenderModeDiscuss}}),
	)
	view, err := builder.Build(ctx, BuildInput{
		Scope:  scope,
		Intent: contextfrag.IntentACPRuntimePrompt,
		Sources: []SourceSpec{{
			Name: "discuss_context",
			Config: DiscussContextConfig{
				RC:             input.RC,
				TRs:            input.TRs,
				CompactSummary: input.CompactSummary,
				LateBinding:    input.LateBinding,
			},
		}},
		Targets: []contextfrag.RenderTarget{contextfrag.RenderACPFullContext},
	})
	if err != nil {
		return "", err
	}
	payload, ok := view.Rendered[contextfrag.RenderACPFullContext].Data.(*ACPRenderedPayload)
	if !ok {
		return "", fmt.Errorf("unexpected acp payload type %T", view.Rendered[contextfrag.RenderACPFullContext].Data)
	}
	return payload.ContextMarkdown, nil
}
