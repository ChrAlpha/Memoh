package runconfig

import (
	"context"
	"log/slog"
	"strings"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/internal/botenv"
	"github.com/felinics/memoh/internal/agent/internal/transcript"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/hooks"
)

// prepareRunConfig generates the system prompt and appends the user message.
func (b *Builder) Prepare(ctx context.Context, cfg native.RunConfig) native.RunConfig {
	cfg.ContextHookText = ""
	beforePromptResult := b.bot.RunPromptHook(ctx, botenv.PromptView{
		BotID:        cfg.Identity.BotID,
		SessionID:    cfg.Identity.SessionID,
		ChatID:       cfg.Identity.ChatID,
		SessionType:  cfg.SessionType,
		MessageCount: len(cfg.Messages),
	}, hooks.EventBeforePromptBuild)
	beforePromptContext := beforePromptResult.AppendContext
	limits := b.agent.Limits()
	nowFn := time.Now
	if cfg.Identity.TimezoneLocation != nil {
		nowFn = func() time.Time { return time.Now().In(cfg.Identity.TimezoneLocation) }
	}
	fs := native.NewFSClient(b.agent.BridgeProvider(), cfg.Identity.BotID, nowFn)
	b.overview.Refresh(ctx, cfg.Identity.BotID, cfg.Identity.SessionID, cfg.SessionType, cfg.PromptCacheTTL)
	files := fs.LoadSystemFiles(ctx)

	platformIdentitiesSection := ""
	var platformIdentityItems []native.SystemPromptItem
	identities, err := b.identities.ListPlatformIdentities(ctx, cfg.Identity.BotID)
	if err != nil {
		b.logger.WarnContext(ctx, "load bot platform identities failed",
			slog.String("bot_id", cfg.Identity.BotID),
			slog.Any("error", err),
		)
	} else {
		platformIdentityItems = buildPlatformIdentityPromptItems(identities)
		platformIdentitiesSection = buildPlatformIdentitiesSectionFromItems(platformIdentityItems)
	}
	systemParams := native.SystemPromptParams{
		SessionType:               cfg.SessionType,
		ReplyDelivery:             cfg.ReplyDelivery,
		Platform:                  cfg.Identity.CurrentPlatform,
		Bot:                       cfg.Bot,
		Skills:                    cfg.Skills,
		Files:                     files,
		MaxFilesBytes:             limits.SystemFilesMaxBytes,
		Timezone:                  cfg.Identity.Timezone,
		PlatformIdentitiesSection: platformIdentitiesSection,
		PlatformIdentities:        platformIdentityItems,
	}
	cfg.System = native.GenerateSystemPrompt(systemParams)
	var promptHookTexts []string
	if beforePromptContext != "" {
		text := botenv.FormatHookContext(hooks.EventBeforePromptBuild, beforePromptContext)
		promptHookTexts = append(promptHookTexts, text)
	}
	beforePromptObservedTexts := append([]string(nil), promptHookTexts...)
	beforePromptObservedTexts = append(beforePromptObservedTexts, hookSystemSectionTexts(beforePromptResult)...)
	afterPromptResult := b.bot.RunPromptHook(ctx, botenv.PromptView{
		BotID:        cfg.Identity.BotID,
		SessionID:    cfg.Identity.SessionID,
		ChatID:       cfg.Identity.ChatID,
		SessionType:  cfg.SessionType,
		MessageCount: len(cfg.Messages),
		SystemBytes:  botenv.AfterPromptSystemBytes(cfg.System, beforePromptObservedTexts),
	}, hooks.EventAfterPromptBuild)
	afterPromptContext := afterPromptResult.AppendContext
	if afterPromptContext != "" {
		text := botenv.FormatHookContext(hooks.EventAfterPromptBuild, afterPromptContext)
		promptHookTexts = append(promptHookTexts, text)
	}
	cfg.ContextHookText = strings.Join(promptHookTexts, "\n\n")

	if cfg.Query != "" {
		var extra []sdk.MessagePart
		for _, img := range cfg.InlineImages {
			if strings.TrimSpace(img.Image) != "" {
				extra = append(extra, img)
			}
		}
		extra = append(extra, cfg.InlineAttachments...)
		cfg.Messages = append(cfg.Messages, sdk.UserMessage(cfg.Query, extra...))
		cfg.ForkContextSourceMessageIDs = append(cfg.ForkContextSourceMessageIDs, "")
		index := len(cfg.Messages) - 1
		cfg.ContextCurrentUserMessageIndex = &index
		cfg.ContextQueryMaterialized = true
	} else if len(cfg.InlineImages) > 0 || len(cfg.InlineAttachments) > 0 {
		// Pipeline path: the user query is already embedded in the RC messages,
		// but media parts are not rendered by the pipeline renderer. Inject the
		// inline media into the identified current input so the model receives them.
		imageParts := make([]sdk.MessagePart, 0, len(cfg.InlineImages)+len(cfg.InlineAttachments))
		for _, img := range cfg.InlineImages {
			if strings.TrimSpace(img.Image) != "" {
				imageParts = append(imageParts, img)
			}
		}
		imageParts = append(imageParts, cfg.InlineAttachments...)
		if len(imageParts) > 0 {
			currentIndex := transcript.ValidCurrentUserMessageIndex(
				cfg.Messages,
				cfg.ContextCurrentUserMessageIndex,
				cfg.ContextMemoryMessageIndex,
			)
			if currentIndex != nil {
				i := *currentIndex
				cfg.Messages[i].Content = append(cfg.Messages[i].Content, imageParts...)
				if i < len(cfg.ForkContextSourceMessageIDs) {
					cfg.ForkContextSourceMessageIDs[i] = ""
				}
			} else {
				cfg.Messages = append(cfg.Messages, sdk.UserMessage("", imageParts...))
				cfg.ForkContextSourceMessageIDs = append(cfg.ForkContextSourceMessageIDs, "")
				index := len(cfg.Messages) - 1
				currentIndex = &index
			}
			cfg.ContextCurrentUserMessageIndex = currentIndex
			cfg.ContextQueryMaterialized = true
		}
	}

	hookBuild := buildHookSystemSections([]promptHookOutput{
		{Event: hooks.EventBeforePromptBuild, Result: beforePromptResult},
		{Event: hooks.EventAfterPromptBuild, Result: afterPromptResult},
	}, cfg.ContextScope)
	cfg.ContextSourceFrags = buildProviderSourceFrags(
		ctx,
		cfg,
		native.GenerateSystemSections(systemParams),
		hookBuild.Frags,
	)
	cfg.ContextSourceWarnings = hookBuild.Warnings
	return cfg.RefreshContextFrag()
}
