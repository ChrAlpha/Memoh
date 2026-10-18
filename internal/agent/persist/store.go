package persist

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runreq"
	turnpkg "github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

type RoundOptions struct {
	AllowPendingToolCalls             bool
	SkipMemory                        bool
	AllowEmptyAssistantText           bool
	MessageMetadataByIndex            map[int]map[string]any
	ReasoningTiming                   []messagepkg.ReasoningTimingSegment
	RequireCompletePersist            bool
	CleanupRuntimeDecisionProjections bool
	AgentPublication                  *messagepkg.AgentPublication
	AgentTurnID                       string
	ContextLifecycle                  *contextfrag.LifecycleHolder
}

func (w *Writer) StoreRound(ctx context.Context, req runreq.ChatRequest, messages []runreq.ModelMessage, modelID string, opts RoundOptions) error {
	_, err := w.StoreRoundResult(ctx, req, messages, modelID, opts)
	return err
}

func (w *Writer) StoreRoundResult(ctx context.Context, req runreq.ChatRequest, messages []runreq.ModelMessage, modelID string, opts RoundOptions) ([]messagepkg.Message, error) {
	fullRound := make([]runreq.ModelMessage, 0, len(messages))

	// When the user message was already persisted by a channel adapter, skip
	// the duplicate from the round. Otherwise keep it so that user + assistant
	// messages are written atomically (deferred persistence).
	skipUserQuery := req.UserMessagePersisted || req.ReusePersistedUserMessage
	for _, m := range messages {
		if skipUserQuery && m.Role == "user" && strings.TrimSpace(m.TextContent()) == strings.TrimSpace(req.Query) {
			skipUserQuery = false // only skip the first matching user message
			continue
		}
		fullRound = append(fullRound, m)
	}
	if !opts.AllowPendingToolCalls {
		fullRound = runreq.RepairToolCallClosures(fullRound, runreq.SyntheticToolClosureError)
	}

	// Filter out empty assistant messages (content: []) that result from LLM
	// returning no useful output (e.g., context window overflow). These provide
	// no value and pollute the conversation history, causing subsequent turns
	// to also produce empty responses.
	filtered := make([]runreq.ModelMessage, 0, len(fullRound))
	for _, m := range fullRound {
		if m.Role == "assistant" && runreq.IsEmptyAssistantMessage(m) && !opts.AllowEmptyAssistantText {
			w.logger.WarnContext(ctx, "skipping empty assistant message in storeRound",
				slog.String("bot_id", req.BotID),
			)
			continue
		}
		filtered = append(filtered, m)
	}

	if len(filtered) == 0 {
		return nil, nil
	}
	opts = opts.withContextLifecycleMetadata(w.logger, req, filtered)

	persisted, persistErr := w.storeMessagesResult(ctx, req, filtered, modelID, opts)
	opts.ContextLifecycle.SetAssistantMessageID(runreq.LastPersistedAssistantMessageID(persisted))
	if persistErr != nil {
		return persisted, persistErr
	}
	if len(persisted) != len(filtered) {
		if opts.RequireCompletePersist {
			return persisted, fmt.Errorf("persisted %d of %d messages", len(persisted), len(filtered))
		}
		w.logger.WarnContext(ctx, "skipping memory extraction for partially persisted round",
			slog.String("bot_id", req.BotID),
			slog.Int("persisted", len(persisted)),
			slog.Int("expected", len(filtered)),
		)
		return persisted, nil
	}
	if !opts.SkipMemory && !req.SkipMemoryExtraction {
		w.rounds.RoundPersisted(context.WithoutCancel(ctx), req, persisted)
	}

	return persisted, nil
}

func (opts RoundOptions) withContextLifecycleMetadata(logger *slog.Logger, req runreq.ChatRequest, messages []runreq.ModelMessage) RoundOptions {
	snapshot, ok := opts.ContextLifecycle.Snapshot()
	if !ok {
		reason := "missing_snapshot"
		if opts.ContextLifecycle == nil {
			reason = "missing_lifecycle"
		}
		logContextLifecycleMetadataSkipped(logger, req, reason, len(messages))
		return opts
	}
	idx := lastAssistantMessageIndex(messages)
	if idx < 0 {
		logContextLifecycleMetadataSkipped(logger, req, "missing_assistant", len(messages))
		return opts
	}
	if opts.MessageMetadataByIndex == nil {
		opts.MessageMetadataByIndex = make(map[int]map[string]any, 1)
	}
	existing := opts.MessageMetadataByIndex[idx]
	if existing == nil {
		existing = map[string]any{}
	}
	existing[contextfrag.MetadataContextLifecycleKey] = snapshot.Summary()
	opts.MessageMetadataByIndex[idx] = existing
	return opts
}

func logContextLifecycleMetadataSkipped(logger *slog.Logger, req runreq.ChatRequest, reason string, messages int) {
	logger.Debug("context lifecycle metadata not persisted",
		slog.String("reason", reason),
		slog.String("bot_id", req.BotID),
		slog.String("session_id", req.ThreadID),
		slog.Int("messages", messages),
	)
}

func lastAssistantMessageIndex(messages []runreq.ModelMessage) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(messages[i].Role), "assistant") {
			return i
		}
	}
	return -1
}

func (w *Writer) storeMessagesResult(ctx context.Context, req runreq.ChatRequest, messages []runreq.ModelMessage, modelID string, opts RoundOptions) ([]messagepkg.Message, error) {
	if strings.TrimSpace(req.BotID) == "" {
		return nil, nil
	}
	persistInputs, err := w.buildPersistInputs(ctx, req, messages, modelID, opts)
	if err != nil {
		return nil, fmt.Errorf("prepare messages for persistence: %w", err)
	}
	// Persist the complete round and its runtime publication in one
	// transaction so warm processes observe only committed rounds.
	if opts.RequireCompletePersist {
		atomic, ok := w.messages.(messagepkg.AtomicRoundPersister)
		if !ok {
			return nil, errors.New("complete round persistence requires an atomic message persister")
		}
		persisted, handled, persistErr := atomic.PersistRound(ctx, persistInputs, messagepkg.RoundPersistenceOptions{
			CleanupRuntimeDecisionProjections: opts.CleanupRuntimeDecisionProjections,
			AgentPublication:                  opts.AgentPublication,
			AgentTurnID:                       opts.AgentTurnID,
		})
		if persistErr != nil {
			return persisted, persistErr
		}
		if !handled {
			return nil, errors.New("atomic message persister declined complete round persistence")
		}
		return persisted, nil
	}
	if batcher, ok := w.messages.(messagepkg.ToolTailRoundPersister); ok {
		if persisted, handled, err := batcher.PersistToolTailRound(ctx, persistInputs); handled || err != nil {
			if err != nil {
				return nil, fmt.Errorf("persist tool tail round: %w", err)
			}
			return persisted, nil
		}
	}
	turnRequestMessageID := ""
	if req.UserMessagePersisted || req.ReusePersistedUserMessage {
		turnRequestMessageID = strings.TrimSpace(req.PersistedUserMessageID)
	}
	return w.persistMessageInputs(ctx, persistInputs, turnRequestMessageID), nil
}

func (w *Writer) buildPersistInputs(ctx context.Context, req runreq.ChatRequest, messages []runreq.ModelMessage, modelID string, opts RoundOptions) ([]messagepkg.PersistInput, error) {
	// Project timing only at the final persistence boundary, after callers have
	// finished filtering, repairing, or augmenting the rows being stored.
	opts = opts.withReasoningTimingMetadata(messages)
	// Check bot setting for full tool result persistence.
	pruneToolResults := true
	if botSettings, err := w.botenv.LoadSettings(ctx, req.BotID); err == nil {
		pruneToolResults = !botSettings.PersistFullToolResults
	}
	meta := RouteMetadata(req)
	meta = MergeMetadata(meta, workspaceTargetMetadata(req.WorkspaceTarget))
	senderChannelIdentityID, senderUserID := w.SenderIDs(ctx, req)
	sessionMode, runtimeType := w.SessionRuntimeSnapshot(ctx, req)

	// Determine the last assistant message index for outbound asset attachment.
	lastAssistantIdx := -1
	if req.OutboundAssetCollector != nil {
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == "assistant" {
				lastAssistantIdx = i
				break
			}
		}
	}
	var outboundAssets []messagepkg.AssetRef
	if lastAssistantIdx >= 0 {
		outboundAssets = runreq.OutboundAssetRefsToMessageRefs(req.OutboundAssetCollector())
	}

	turnRequestMessageID := ""
	if req.UserMessagePersisted || req.ReusePersistedUserMessage {
		turnRequestMessageID = strings.TrimSpace(req.PersistedUserMessageID)
	}
	persistInputs := make([]messagepkg.PersistInput, 0, len(messages))
	originalQueryAvailable := !req.UserMessagePersisted && !req.ReusePersistedUserMessage
	for i, msg := range messages {
		msg = runreq.NormalizeUserMessageContent(msg)

		// Prune tool results at store time to reduce DB bloat.
		// This prevents ~10KB+ tool outputs from being stored verbatim.
		if pruneToolResults {
			if pruned, changed := pruneMessageForGateway(msg); changed {
				msg = pruned
			}
		}

		// Lift UI-only diffs out of tool-call providerMetadata before the
		// content column is written: inside content they would count against
		// the history byte budget; on the row's metadata they still reach the
		// UI converter via ToolCallDiffsMetadataKey.
		msg, toolCallDiffs := historyfrag.ExtractToolCallDiffs(msg)

		content, err := historyfrag.MarshalStoredModelMessage(msg)
		if err != nil {
			return nil, fmt.Errorf("marshal message %d: %w", i, err)
		}
		messageSenderChannelIdentityID := ""
		messageSenderUserID := ""
		externalMessageID := ""
		sourceReplyToMessageID := ""
		messageEventID := ""
		displayText := ""
		assets := []messagepkg.AssetRef(nil)
		persistMeta := meta
		// The admitted turn belongs to exactly one row: the turn-leading user
		// query. Everything else in the round either binds to it or is a
		// synthetic mid-turn message that must not claim the turn's identity.
		turnID := ""
		var turnPosition *int64
		isInternalFeedback := msg.Source == turnpkg.MessageSourceInternalFeedback
		if msg.Role == "user" && !isInternalFeedback {
			messageSenderChannelIdentityID = senderChannelIdentityID
			messageSenderUserID = senderUserID

			// Apply the inbound event and display metadata only to its original
			// query; steered user messages retain their own text. Internal model
			// feedback bypasses this user-message path based on its source.
			ownText := strings.TrimSpace(msg.TextContent())
			isOriginalSkillActivation := req.UserMessageKind == runreq.UserMessageKindSkillActivation &&
				strings.TrimSpace(req.Query) == "" &&
				ownText == "" &&
				i == 0
			isOriginalQuery := originalQueryAvailable && ((ownText != "" && ownText == strings.TrimSpace(req.Query)) || isOriginalSkillActivation)

			if isOriginalQuery {
				originalQueryAvailable = false
				externalMessageID = req.ExternalMessageID
				sourceReplyToMessageID = req.SourceReplyToMessageID
				messageEventID = req.EventID
				turnID = req.TurnID
				turnPosition = req.TurnPosition
				switch {
				case strings.TrimSpace(req.UserVisibleText) != "" || req.UserMessageKind == runreq.UserMessageKindSkillActivation:
					displayText = strings.TrimSpace(req.UserVisibleText)
				case req.RawQuery != "":
					displayText = req.RawQuery
				default:
					displayText = strings.TrimSpace(req.Query)
				}
				// req.Query is the headerified <message> envelope by the time a
				// round is stored, and retry copies that envelope into RawQuery.
				// An attachment-only message (no caption) has no raw text to fall
				// back on, so without this the wrapper itself became the user
				// bubble. Real user text is left untouched.
				displayText = strings.TrimSpace(turnpkg.UnwrapUserMessageEnvelope(displayText))
				assets = runreq.ChatAttachmentsToAssetRefs(req.Attachments)
				persistMeta = MergeMetadata(meta, InteractionMetadata(req))
			} else {
				displayText = ownText
			}
		} else if !isInternalFeedback && strings.TrimSpace(req.ExternalMessageID) != "" {
			sourceReplyToMessageID = req.ExternalMessageID
		}
		if i == lastAssistantIdx && len(outboundAssets) > 0 {
			assets = append(assets, outboundAssets...)
		}
		if len(toolCallDiffs) > 0 {
			persistMeta = MergeMetadata(persistMeta, map[string]any{
				messagepkg.ToolCallDiffsMetadataKey: toolCallDiffs,
			})
		}
		if isInternalFeedback {
			persistMeta = MergeMetadata(persistMeta, map[string]any{messagepkg.MessageSourceMetadataKey: messagepkg.MessageSourceInternalFeedback})
			displayText = ""
		}
		if extraMeta := opts.MessageMetadataByIndex[i]; len(extraMeta) > 0 {
			persistMeta = MergeMetadata(persistMeta, extraMeta)
		}
		persistInputs = append(persistInputs, messagepkg.PersistInput{
			BotID:                   req.BotID,
			SessionID:               req.ThreadID,
			SenderChannelIdentityID: messageSenderChannelIdentityID,
			SenderUserID:            messageSenderUserID,
			ExternalMessageID:       externalMessageID,
			SourceReplyToMessageID:  sourceReplyToMessageID,
			Role:                    msg.Role,
			Content:                 content,
			Metadata:                persistMeta,
			Usage:                   msg.Usage,
			Assets:                  assets,
			ModelID:                 modelID,
			EventID:                 messageEventID,
			DisplayText:             displayText,
			SessionMode:             sessionMode,
			RuntimeType:             runtimeType,
			TurnRequestMessageID:    turnRequestMessageID,
			SkipHistoryTurn:         req.SkipHistoryTurn,
			// Assistant and tool rows bind to the request message's turn, so they
			// carry the run for traceability but never a turn of their own.
			RunID:        req.RunID,
			TurnID:       turnID,
			TurnPosition: turnPosition,
		})
	}
	return persistInputs, nil
}

func workspaceTargetMetadata(target *runreq.WorkspaceTarget) map[string]any {
	if target == nil || strings.TrimSpace(target.TargetID) == "" {
		return nil
	}
	return map[string]any{
		"execution_location": map[string]any{
			"target_id": strings.TrimSpace(target.TargetID),
			"kind":      strings.TrimSpace(target.Kind),
			"name":      strings.TrimSpace(target.Name),
		},
	}
}

func (w *Writer) persistSessionWorkspaceTarget(ctx context.Context, req runreq.ChatRequest) error {
	if req.WorkspaceTarget == nil || strings.TrimSpace(req.ThreadID) == "" {
		return nil
	}
	sess, err := w.sessions.Get(ctx, req.ThreadID)
	if err != nil {
		return err
	}
	metadata := make(map[string]any, len(sess.Metadata)+2)
	for key, value := range sess.Metadata {
		metadata[key] = value
	}
	target := req.WorkspaceTarget
	metadata["workspace_target_id"] = strings.TrimSpace(target.TargetID)
	metadata["workspace_target"] = map[string]any{
		"target_id": strings.TrimSpace(target.TargetID),
		"kind":      strings.TrimSpace(target.Kind),
		"name":      strings.TrimSpace(target.Name),
	}
	_, err = w.sessions.UpdateMetadata(ctx, req.ThreadID, metadata)
	return err
}

func (w *Writer) persistMessageInputs(ctx context.Context, inputs []messagepkg.PersistInput, initialTurnRequestMessageID string) []messagepkg.Message {
	persisted := make([]messagepkg.Message, 0, len(inputs))
	turnRequestMessageID := strings.TrimSpace(initialTurnRequestMessageID)
	for _, input := range inputs {
		if !input.SkipHistoryTurn {
			input.TurnRequestMessageID = turnRequestMessageID
		}
		persistedMessage, err := w.messages.Persist(ctx, input)
		if err != nil {
			w.logger.WarnContext(ctx, "persist message failed", slog.Any("error", err))
			continue
		}
		if strings.EqualFold(strings.TrimSpace(input.Role), "user") && !input.SkipHistoryTurn && !messagepkg.IsInternalFeedback(input.Metadata) {
			turnRequestMessageID = persistedMessage.ID
		}
		persisted = append(persisted, persistedMessage)
	}
	return persisted
}

func (w *Writer) SessionRuntimeSnapshot(ctx context.Context, req runreq.ChatRequest) (string, string) {
	sessionMode := strings.TrimSpace(req.SessionType)
	runtimeType := strings.TrimSpace(req.RuntimeType)
	if sessionMode != "" && runtimeType != "" {
		return sessionMode, runtimeType
	}
	if strings.TrimSpace(req.ThreadID) != "" {
		if sess, err := w.sessions.Get(ctx, req.ThreadID); err == nil {
			if sessionMode == "" {
				sessionMode = strings.TrimSpace(sess.SessionMode)
			}
			if runtimeType == "" {
				runtimeType = strings.TrimSpace(sess.RuntimeType)
			}
		}
	}
	if sessionMode == "" {
		sessionMode = "chat"
	}
	if runtimeType == "" {
		runtimeType = "model"
	}
	return sessionMode, runtimeType
}

func RouteMetadata(req runreq.ChatRequest) map[string]any {
	if strings.TrimSpace(req.RouteID) == "" && strings.TrimSpace(req.CurrentChannel) == "" {
		return nil
	}
	meta := map[string]any{}
	if strings.TrimSpace(req.RouteID) != "" {
		meta["route_id"] = req.RouteID
	}
	if strings.TrimSpace(req.CurrentChannel) != "" {
		meta["platform"] = req.CurrentChannel
	}
	return meta
}

func InteractionMetadata(req runreq.ChatRequest) map[string]any {
	meta := map[string]any{}
	reply := map[string]any{}
	if v := strings.TrimSpace(req.SourceReplyToMessageID); v != "" {
		reply["message_id"] = v
	}
	if v := strings.TrimSpace(req.ReplySender); v != "" {
		reply["sender"] = v
	}
	if v := strings.TrimSpace(req.ReplyPreview); v != "" {
		reply["preview"] = v
	}
	if attachments := chatAttachmentMetadata(req.ReplyAttachments); len(attachments) > 0 {
		reply["attachments"] = attachments
	}
	if len(reply) > 0 {
		meta["reply"] = reply
	}

	forward := map[string]any{}
	if v := strings.TrimSpace(req.ForwardMessageID); v != "" {
		forward["message_id"] = v
	}
	if v := strings.TrimSpace(req.ForwardFromUserID); v != "" {
		forward["from_user_id"] = v
	}
	if v := strings.TrimSpace(req.ForwardFromConversationID); v != "" {
		forward["from_conversation_id"] = v
	}
	if v := strings.TrimSpace(req.ForwardSender); v != "" {
		forward["sender"] = v
	}
	if req.ForwardDate > 0 {
		forward["date"] = req.ForwardDate
	}
	if len(forward) > 0 {
		meta["forward"] = forward
	}
	if requestedSkills := publicRequestedSkillMetadata(req.RequestedSkills); len(requestedSkills) > 0 {
		meta["model_requested_skills"] = requestedSkills
	}
	if kind := strings.TrimSpace(req.UserMessageKind); kind != "" {
		meta["user_message_kind"] = kind
	}
	if activation := publicSkillActivationMetadata(req.SkillActivation); activation != nil {
		meta["skill_activation"] = activation
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}

func publicSkillActivationMetadata(activation *runreq.SkillActivation) map[string]any {
	if activation == nil {
		return nil
	}
	out := map[string]any{}
	if prompt := strings.TrimSpace(activation.Prompt); prompt != "" {
		out["prompt"] = prompt
	}
	skills := make([]map[string]any, 0, len(activation.Skills))
	for _, skill := range activation.Skills {
		name := strings.TrimSpace(skill.Name)
		if name == "" {
			continue
		}
		item := map[string]any{"name": name}
		if displayName := strings.TrimSpace(skill.DisplayName); displayName != "" {
			item["display_name"] = displayName
		}
		if description := strings.TrimSpace(skill.Description); description != "" {
			item["description"] = description
		}
		if sourceKind := strings.TrimSpace(skill.SourceKind); sourceKind != "" {
			item["source_kind"] = sourceKind
		}
		if state := strings.TrimSpace(skill.State); state != "" {
			item["state"] = state
		}
		skills = append(skills, item)
	}
	if len(skills) > 0 {
		out["skills"] = skills
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func publicRequestedSkillMetadata(items []runreq.RequestedSkillContext) []map[string]any {
	if len(items) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	seen := map[string]struct{}{}
	for _, item := range items {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			continue
		}
		key := strings.TrimSpace(item.Identity)
		if key == "" {
			key = name + "\x00" + strings.TrimSpace(item.SourceKind) + "\x00" + strings.TrimSpace(item.OpaqueSourceID)
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		entry := map[string]any{"name": name}
		if sourceKind := strings.TrimSpace(item.SourceKind); sourceKind != "" {
			entry["source_kind"] = sourceKind
		}
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func chatAttachmentMetadata(attachments []runreq.ChatAttachment) []map[string]any {
	if len(attachments) == 0 {
		return nil
	}
	result := make([]map[string]any, 0, len(attachments))
	for _, att := range attachments {
		item := att.Bundle().ToMap()
		if len(item) > 0 {
			result = append(result, item)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func MergeMetadata(base, extra map[string]any) map[string]any {
	if len(extra) == 0 {
		return base
	}
	merged := map[string]any{}
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range extra {
		merged[key] = value
	}
	return merged
}

func (w *Writer) SenderIDs(ctx context.Context, req runreq.ChatRequest) (string, string) {
	channelIdentityID := strings.TrimSpace(req.SourceChannelIdentityID)
	userID := strings.TrimSpace(req.UserID)

	senderChannelIdentityID := ""
	if w.botenv.IsExistingChannelIdentity(ctx, channelIdentityID) {
		senderChannelIdentityID = channelIdentityID
	}

	senderUserID := ""
	if w.botenv.IsExistingUser(ctx, userID) {
		senderUserID = userID
	}
	return senderChannelIdentityID, senderUserID
}

// LinkOutboundAssets links bot-generated assets to the assistant message that
// produced them. Assets carrying a `tool_call_id` metadata entry are anchored
// to the assistant message containing that tool call, so a history rebuild
// keeps the live ordering (e.g. a generated image stays above the closing
// text). Assets without one fall back to the latest assistant message. When
// sessionID is provided, the search is scoped to that session; otherwise it
// falls back to a bot-wide search.
// Used by the WebSocket path where attachment ingestion happens after message
// persistence.
func (w *Writer) LinkOutboundAssets(ctx context.Context, botID, sessionID string, assets []messagepkg.AssetRef) {
	if len(assets) == 0 || strings.TrimSpace(botID) == "" {
		return
	}
	var (
		msgs []messagepkg.Message
		err  error
	)
	// A single turn can span many rows (the originating tool-call assistant
	// message, its tool-result row, and any follow-up assistant/tool messages
	// such as sending the image to several channels). The window must comfortably
	// cover a whole turn so tool-call anchoring below finds the originating
	// message instead of silently falling back to the latest assistant message.
	const anchorSearchWindow = 50
	if strings.TrimSpace(sessionID) != "" {
		msgs, err = w.messages.ListLatestBySession(ctx, sessionID, anchorSearchWindow)
	} else {
		msgs, err = w.messages.ListLatest(ctx, botID, anchorSearchWindow)
	}
	if err != nil {
		w.logger.WarnContext(ctx, "LinkOutboundAssets: list latest failed", slog.Any("error", err))
		return
	}

	latestAssistantID := ""
	for _, msg := range msgs {
		if msg.Role == "assistant" {
			latestAssistantID = msg.ID
			break
		}
	}
	if latestAssistantID == "" {
		w.logger.WarnContext(ctx, "LinkOutboundAssets: no assistant message found", slog.String("bot_id", botID))
		return
	}

	byMessage := map[string][]messagepkg.AssetRef{}
	var order []string
	for _, asset := range assets {
		targetID := latestAssistantID
		if toolCallID, _ := asset.Metadata["tool_call_id"].(string); strings.TrimSpace(toolCallID) != "" {
			if id := findAssistantMessageForToolCall(msgs, strings.TrimSpace(toolCallID)); id != "" {
				targetID = id
			} else {
				w.logger.DebugContext(ctx, "LinkOutboundAssets: tool call not found in search window, anchoring to latest assistant",
					slog.String("tool_call_id", strings.TrimSpace(toolCallID)))
			}
		}
		if _, ok := byMessage[targetID]; !ok {
			order = append(order, targetID)
		}
		byMessage[targetID] = append(byMessage[targetID], asset)
	}
	for _, id := range order {
		group := byMessage[id]
		for i := range group {
			group[i].Ordinal = i
		}
		if linkErr := w.messages.LinkAssets(ctx, id, group); linkErr != nil {
			w.logger.WarnContext(ctx, "LinkOutboundAssets: link failed", slog.Any("error", linkErr))
		}
	}
}

// findAssistantMessageForToolCall returns the ID of the assistant message
// whose serialized content contains the given tool call ID as an exact JSON
// string token. Tool call IDs are unique opaque tokens, so a quoted match
// cannot collide with ordinary reply text.
func findAssistantMessageForToolCall(msgs []messagepkg.Message, toolCallID string) string {
	needle := `"` + toolCallID + `"`
	for _, msg := range msgs {
		if msg.Role != "assistant" || len(msg.Content) == 0 {
			continue
		}
		if strings.Contains(string(msg.Content), needle) {
			return msg.ID
		}
	}
	return ""
}

// consumePromoted records, once a run has applied a steer, that the input it
// was promoted from is taken. A steer that was not promoted from a queued input
// matches nothing. A failure leaves the record to the sweep, which reads the
// steer.
func (w *Writer) consumePromoted(ctx context.Context, steerID string) {
	if err := w.promoted.ConsumePromoted(context.WithoutCancel(ctx), steerID); err != nil {
		w.logger.WarnContext(ctx, "record applied promoted input failed",
			slog.String("item_id", steerID), slog.Any("error", err))
	}
}
