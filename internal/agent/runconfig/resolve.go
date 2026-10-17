package runconfig

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/compact"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/agent/transcript"
	turnpkg "github.com/felinics/memoh/internal/agent/turn"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/models"
)

// effectiveContextTokenBudget derives the per-turn context budget: the model
// context window, or the configured fallback when the model has none, so a
// missing window never leaves the context unbounded.
func (b *Builder) effectiveContextTokenBudget(chatModel models.GetResponse) int {
	budget := contextBudgetFromChatModel(chatModel)
	if budget > 0 {
		return budget
	}
	fallback := b.transcript.AbsoluteMaxTokens()
	b.logger.Warn("context budget: model context window missing, applying fallback",
		slog.String("model_id", chatModel.ID),
		slog.Int("fallback_tokens", fallback))
	return fallback
}

func contextBudgetFromChatModel(chatModel models.GetResponse) int {
	return chatModel.Config.ContextBudgetMaxTokens()
}

func markRequiredHistoryMessageCurrent(cfg *native.RunConfig, requiredMessageID string) {
	if cfg == nil {
		return
	}
	cfg.ContextCurrentUserMessageIndex = nil
	requiredMessageID = strings.TrimSpace(requiredMessageID)
	if requiredMessageID == "" {
		return
	}
	for index, sourceMessageID := range cfg.ForkContextSourceMessageIDs {
		if strings.TrimSpace(sourceMessageID) != requiredMessageID {
			continue
		}
		if index >= len(cfg.Messages) || cfg.Messages[index].Role != sdk.MessageRoleUser {
			return
		}
		cfg.ContextCurrentUserMessageIndex = runreq.IntPointer(index)
		return
	}
}

// Resolve builds the run context for one turn and returns the effective
// request alongside it. It only reads: the session's model preference is
// written back, and the running input channel connected, by the caller once
// resolution has succeeded (ResolvedContext.PairCarried says whether the write
// is due). Resolution fills in defaults the caller's copy does not
// have — a direct turn on a subagent thread learns its session type, pinned
// model, and skip flags here — so everything that runs after resolution (title
// generation, the step committer, terminal persistence) must use the returned
// request, not the one that went in. Query is deliberately untouched: callers
// that persist a headerified user message still take it from runreq.ResolvedContext.
func (b *Builder) Resolve(ctx context.Context, req runreq.ChatRequest) (runreq.ResolvedContext, runreq.ChatRequest, error) {
	modelQuery := runreq.ModelQueryText(req)
	// A request whose input is already persisted carries no query of its own:
	// its current message is the history row it names.
	if strings.TrimSpace(modelQuery) == "" && len(req.Attachments) == 0 && !req.ReusePersistedUserMessage {
		return runreq.ResolvedContext{}, req, errors.New("query or attachments is required")
	}
	if strings.TrimSpace(req.BotID) == "" {
		return runreq.ResolvedContext{}, req, errors.New("bot id is required")
	}
	if strings.TrimSpace(req.ChatID) == "" {
		return runreq.ResolvedContext{}, req, errors.New("chat id is required")
	}
	if err := b.RejectRequestedSkillsIfUnsupportedContext(ctx, req); err != nil {
		return runreq.ResolvedContext{}, req, err
	}
	// Session model preference (issue #879 spec v2): loaded once per turn,
	// BEFORE the subagent pin below — the pin is only a session's initial
	// default and must not refill the request over a remembered pair.
	// requestCarriesPair is captured before that mutation so the write-back
	// gate sees the request as it actually arrived: the web composer omits
	// model/effort when the pair is default-sourced, and channel requests
	// structurally never carry them (only the local REST handler ever fills
	// the metadata keys), so "carries" == "explicitly chosen or remembered".
	// Schedule-triggered turns skip the memory level entirely: their model
	// comes from the schedule payload or the bot default, and a pair
	// remembered from a web continuation must not hijack them.
	requestCarriesPair := strings.TrimSpace(req.Model) != "" || strings.TrimSpace(req.ReasoningEffort) != ""
	sessionPrefModelID, sessionPrefEffort := "", ""
	pairMemoryApplies := !strings.EqualFold(strings.TrimSpace(req.SessionType), sessionmode.Schedule)
	if pairMemoryApplies {
		sessionPrefModelID, sessionPrefEffort = b.sessionModelPreference(ctx, req.ThreadID)
	}
	req = b.transcript.ApplySubagentDefaults(ctx, req, sessionPrefModelID != "")
	facts := b.requestRunFacts(ctx, req)

	runCfg, chatModel, provider, err := b.buildBase(ctx, baseParams{
		BotID:              req.BotID,
		ChatID:             req.ChatID,
		SessionID:          req.ThreadID,
		RouteID:            req.RouteID,
		UserID:             req.UserID,
		ChannelIdentityID:  req.SourceChannelIdentityID,
		CurrentPlatform:    req.CurrentChannel,
		ReplyTarget:        req.ReplyTarget,
		ConversationType:   req.ConversationType,
		SessionToken:       req.ChatToken,
		SessionType:        req.SessionType,
		Facts:              facts,
		SessionPrefModelID: sessionPrefModelID,
		SessionPrefEffort:  sessionPrefEffort,
		Model:              req.Model,
		Provider:           req.Provider,
		ReasoningEffort:    req.ReasoningEffort,
	})
	if err != nil {
		b.logger.ErrorContext(ctx, "resolve: buildBaseRunConfig failed",
			slog.String("bot_id", req.BotID),
			slog.Any("error", err),
		)
		return runreq.ResolvedContext{}, req, err
	}

	if strings.EqualFold(strings.TrimSpace(req.SessionType), sessionpkg.TypeSubagent) {
		// A direct turn on a subagent thread runs as the subagent, not as a
		// chat turn that happens to share its history: same restricted tool
		// surface (no nested spawns), same prompt mode.
		runCfg.Identity.IsSubagent = true
	}
	runCfg.RunID = runreq.RunID(req.RunID)
	memoryContext := b.memory.Recall(ctx, req)
	if memoryContext.Trace != nil && runCfg.ContextLifecycle != nil {
		runCfg.ContextLifecycle.SetMemoryRecall(*memoryContext.Trace)
	}
	memoryMsg := memoryContext.Message
	reqMessages := runreq.NonNilModelMessages(req.Messages)

	contextTokenBudget := b.effectiveContextTokenBudget(chatModel)
	runCfg.ContextBudgetMaxTokens = contextTokenBudget

	assembled, err := b.transcript.Assemble(ctx, transcript.ForChat(transcript.PurposeRun, req, contextTokenBudget))
	if err != nil {
		b.logger.ErrorContext(ctx, "resolve: assemble context failed",
			slog.String("bot_id", req.BotID),
			slog.String("stage", "initial"),
			slog.Any("error", err),
		)
		return runreq.ResolvedContext{}, req, err
	}
	forkContext := b.transcript.SubagentForkMessages(ctx, req)
	notice := b.transcript.CurrentWorkspaceMessage(ctx, req)
	requestedSkillMsg := buildRequestedSkillContextMessage(req.RequestedSkills)
	var ownMessages []runreq.ModelMessage
	ownMessages = append(ownMessages, forkContext...)
	for _, message := range []*runreq.ModelMessage{notice, memoryMsg, requestedSkillMsg} {
		if message != nil {
			ownMessages = append(ownMessages, *message)
		}
	}
	if !req.ReusePersistedUserMessage {
		ownMessages = append(ownMessages, reqMessages...)
	}
	estimate := b.compact.NewEstimate(ctx, req.ThreadID,
		append(runreq.ModelMessagesToSDKMessages(ownMessages), compact.UserMessageEstimate(req, modelQuery)...))
	assembled, err = b.compact.BeforeTurn(ctx, req, chatModel.ID, contextTokenBudget, assembled, estimate)
	if err != nil {
		b.logger.ErrorContext(ctx, "resolve: assemble context failed",
			slog.String("bot_id", req.BotID),
			slog.String("stage", "post_compaction"),
			slog.Any("error", err),
		)
		return runreq.ResolvedContext{}, req, err
	}
	pressure := runreq.NewRunPressure(runCfg.ContextLifecycle, estimate.Conversation(assembled.Messages))
	messages := assembled.Messages
	historyRecords := assembled.Records
	estimatedTokens := assembled.EstimatedTokens
	if len(forkContext) > 0 {
		// The inherited parent snapshot precedes the thread's own transcript,
		// exactly as parent-driven subagent tasks assemble it.
		messages = append(forkContext, messages...)
	}
	messages = transcript.NormalizeContextMessages(messages)
	trimmableMessages := len(messages)
	if notice != nil {
		messages = append(messages, *notice)
	}
	var memoryMessageIndex *int
	if memoryMsg != nil {
		messages = append(messages, *memoryMsg)
		memoryMessageIndex = runreq.IntPointer(len(messages) - 1)
	}
	if requestedSkillMsg != nil {
		messages = append(messages, *requestedSkillMsg)
	}
	if !req.ReusePersistedUserMessage {
		messages = append(messages, reqMessages...)
	}
	messages = transcript.NormalizeContextMessages(messages)
	runCfg.ContextHistoryTokenEstimates = make([]int, len(messages))
	for index := range messages {
		runCfg.ContextHistoryTokenEstimates[index] = transcript.EstimateMessageTokens(messages[index])
	}
	runCfg.ContextTrimmableMessages = trimmableMessages

	displayName := b.bot.ResolveDisplayName(ctx, req)
	mergedAttachments := b.routeAndMergeAttachments(ctx, chatModel, req)

	tz := runCfg.Identity.TimezoneLocation
	if tz == nil {
		tz = time.UTC
	}
	headerInput := turnpkg.UserMessageHeaderInput{
		MessageID:         strings.TrimSpace(req.ExternalMessageID),
		ChannelIdentityID: strings.TrimSpace(req.SourceChannelIdentityID),
		DisplayName:       displayName,
		Channel:           req.CurrentChannel,
		ConversationType:  strings.TrimSpace(req.ConversationType),
		ConversationName:  strings.TrimSpace(req.ConversationName),
		Target:            strings.TrimSpace(req.ReplyTarget),
		AttachmentPaths:   extractAttachmentPaths(mergedAttachments),
		Time:              time.Now().In(tz),
		Timezone:          runCfg.Identity.Timezone,
		ReplyToMessageID:  strings.TrimSpace(req.SourceReplyToMessageID),
		ReplyToSender:     strings.TrimSpace(req.ReplySender),
		ReplyToPreview:    req.ReplyPreview,
		MentionsMe:        req.MentionsBot,
		RepliesToMe:       req.RepliesToBot,
	}
	headerifiedQuery := ""
	if userQueryNeedsHeader(req, len(mergedAttachments)) {
		headerifiedQuery = turnpkg.FormatUserHeader(headerInput, req.Query)
	}
	headerifiedModelQuery := headerifiedQuery
	if strings.TrimSpace(modelQuery) != strings.TrimSpace(req.Query) {
		headerifiedModelQuery = turnpkg.FormatUserHeader(headerInput, modelQuery)
	}
	runCfg.ContextFrags = transcript.ContextFragsForMessages(messages, historyRecords)
	forkMessages := runreq.NonNilModelMessages(messages)
	runCfg.ForkContextSourceMessageIDs = transcript.SourceMessageIDsForMessages(forkMessages, historyRecords)
	runCfg.Messages = runreq.ModelMessagesToSDKMessages(forkMessages)
	runCfg.ContextMemoryMessageIndex = memoryMessageIndex
	if req.ReusePersistedUserMessage {
		markRequiredHistoryMessageCurrent(&runCfg, req.RequiredHistoryMessageID)
	} else {
		runCfg.Query = headerifiedModelQuery
	}
	runCfg.InlineImages = extractNativeImageParts(mergedAttachments)
	runCfg.InlineAttachments = extractNativeAttachmentParts(mergedAttachments)
	runCfg.ContextScope = buildContextFragScope(req, displayName, runCfg.Identity)
	runCfg = runCfg.RefreshContextFrag()

	return runreq.ResolvedContext{
		RunConfig:          runCfg,
		Model:              chatModel,
		Provider:           provider,
		Query:              headerifiedQuery,
		PairCarried:        requestCarriesPair && pairMemoryApplies,
		EstimatedTokens:    estimatedTokens,
		Pressure:           pressure,
		ContextTokenBudget: contextTokenBudget,
	}, req, nil
}

// userQueryNeedsHeader reports whether resolve should wrap req.Query in the
// user message header (sender/channel/time/attachment paths). Text always
// gets a header. An attachment-only message (no caption) must too: the header
// is the only thing telling the model who sent what file, and the headerified
// query is what makes the user turn (with its asset links) persist to history.
// The one deliberate exception is a no-prompt skill activation, whose stored
// user content must stay empty (its model text travels via ModelQuery, and
// display state via skill_activation metadata).
func userQueryNeedsHeader(req runreq.ChatRequest, attachmentCount int) bool {
	if strings.TrimSpace(req.Query) != "" {
		return true
	}
	return attachmentCount > 0 && req.UserMessageKind != runreq.UserMessageKindSkillActivation
}
