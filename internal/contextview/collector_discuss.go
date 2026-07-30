package contextview

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	pipeline "github.com/memohai/memoh/internal/chat/timeline"
)

const (
	discussContextCollectorName = "discuss_context"
	discussContextSource        = "pipeline_discuss"
)

type DiscussContextConfig struct {
	// ComposedMessages is the authoritative output of timeline composition
	// when non-nil. The RC/TR fields remain for callers that still compose
	// inside this collector.
	ComposedMessages []pipeline.ContextMessage
	RC               pipeline.RenderedContext
	TRs              []pipeline.TurnResponseEntry
	CompactSummary   string
	// LateBinding is the per-turn participation instruction that used to be
	// appended to the message stream after context assembly.
	LateBinding string
	// InlineImages are freshly surfaced RC attachments delivered as native
	// vision input on the latest user message.
	InlineImages []sdk.ImagePart
}

type DiscussContextCollector struct{}

func (*DiscussContextCollector) Name() string {
	return discussContextCollectorName
}

func (*DiscussContextCollector) Collect(_ context.Context, req CollectRequest) ([]contextfrag.ContextFrag, error) {
	cfg, err := discussContextConfig(req.Config)
	if err != nil {
		return nil, err
	}

	if cfg.ComposedMessages == nil &&
		len(cfg.RC) == 0 &&
		len(cfg.TRs) == 0 &&
		cfg.CompactSummary == "" &&
		strings.TrimSpace(cfg.LateBinding) == "" {
		return nil, nil
	}

	var frags []contextfrag.ContextFrag
	if cfg.ComposedMessages != nil {
		frags = make([]contextfrag.ContextFrag, 0, len(cfg.ComposedMessages)+1)
		currentUserIndex := latestComposedUserMessageIndex(cfg.ComposedMessages)
		for i, message := range cfg.ComposedMessages {
			frags = append(frags, discussComposedMessageFrag(message, i, i == currentUserIndex, req.Scope))
		}
	} else {
		entries := sortedDiscussSourceEntries(cfg.RC, cfg.TRs)
		frags = make([]contextfrag.ContextFrag, 0, len(entries)+1)
		if cfg.CompactSummary != "" {
			frags = append(frags, contextfrag.MessageFrag(contextfrag.MessageFragInput{
				ID:         "discuss.summary",
				Message:    sdk.UserMessage("<summary>\n" + cfg.CompactSummary + "\n</summary>"),
				Kind:       contextfrag.KindConversationSummary,
				Slot:       contextfrag.SlotBeforeHistory,
				Priority:   10,
				CacheClass: contextfrag.CacheDynamic,
				Trust:      contextfrag.TrustSystem,
				Scope:      req.Scope,
				Source:     discussContextSource,
				SourceID:   "summary",
				Collector:  discussContextCollectorName,
				Index:      -1,
				Budget:     contextfrag.BudgetPolicy{Overflow: contextfrag.OverflowKeep},
			}))
		}

		for _, entry := range entries {
			switch entry.kind {
			case "rc":
				frag, ok := discussRCFrag(entry.rc, entry.index, req.Scope)
				if ok {
					frags = append(frags, frag)
				}
			case "tr":
				frags = append(frags, discussTRFrag(entry.tr, entry.index, req.Scope))
			}
		}
	}
	if req.Intent == contextfrag.IntentRunConfigPreProvider {
		frags = contextfrag.RepairToolClosureFrags(frags, req.Scope, discussContextCollectorName)
	}
	frags = injectDiscussImages(frags, cfg.InlineImages)
	if lateBinding := strings.TrimSpace(cfg.LateBinding); lateBinding != "" {
		frags = append(frags, discussLateBindingFrag(lateBinding, req.Scope))
	}
	return frags, nil
}

func latestComposedUserMessageIndex(messages []pipeline.ContextMessage) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].CompactionArtifactID == "" &&
			discussContextMessageToSDK(messages[i]).Role == sdk.MessageRoleUser {
			return i
		}
	}
	return -1
}

func discussComposedMessageFrag(message pipeline.ContextMessage, index int, currentUser bool, scope contextfrag.Scope) contextfrag.ContextFrag {
	msg := discussContextMessageToSDK(message)
	input := contextfrag.MessageFragInput{
		ID:         fmt.Sprintf("discuss.message.%03d", index),
		Message:    msg,
		Kind:       contextfrag.KindConversationEvent,
		Slot:       contextfrag.SlotHistory,
		Priority:   contextfrag.PriorityForMessage(msg),
		CacheClass: contextfrag.CacheNever,
		Trust:      trustForDiscussRole(message.Role),
		Scope:      scope,
		Source:     discussContextSource,
		SourceID:   fmt.Sprintf("message.%03d", index),
		Collector:  discussContextCollectorName,
		Index:      index,
	}
	if message.CompactionArtifactID != "" {
		input.Kind = contextfrag.KindConversationSummary
		input.Slot = contextfrag.SlotBeforeHistory
		input.Priority = 10
		input.CacheClass = contextfrag.CacheDynamic
		input.Trust = contextfrag.TrustSystem
		input.Budget = contextfrag.BudgetPolicy{Overflow: contextfrag.OverflowKeep}
	} else if currentUser {
		// Keep the history slot so rendering preserves the authoritative
		// composed order; kind and overflow carry current-request semantics.
		input.Kind = contextfrag.KindCurrentUserMessage
		input.Trust = contextfrag.TrustUser
		input.Budget = contextfrag.BudgetPolicy{Overflow: contextfrag.OverflowKeep}
	}
	return contextfrag.MessageFrag(input)
}

// injectDiscussImages mirrors the legacy inject-into-last-user-message
// behavior at fragment granularity: the freshest user-visible message carries
// the native image parts; without a user message the images are dropped.
func injectDiscussImages(frags []contextfrag.ContextFrag, images []sdk.ImagePart) []contextfrag.ContextFrag {
	extra := make([]sdk.MessagePart, 0, len(images))
	for _, img := range images {
		if strings.TrimSpace(img.Image) != "" {
			extra = append(extra, img)
		}
	}
	if len(extra) == 0 {
		return frags
	}
	for i := len(frags) - 1; i >= 0; i-- {
		msg := discussFragMessage(frags[i])
		if msg == nil || msg.Role != sdk.MessageRoleUser {
			continue
		}
		enriched := *msg
		enriched.Content = append(append([]sdk.MessagePart(nil), msg.Content...), extra...)
		frags[i] = contextfrag.RebuildFragMessage(frags[i], enriched)
		return frags
	}
	return frags
}

func discussFragMessage(frag contextfrag.ContextFrag) *sdk.Message {
	return contextfrag.FragMessage(frag)
}

func discussLateBindingFrag(lateBinding string, scope contextfrag.Scope) contextfrag.ContextFrag {
	msg := sdk.UserMessage(lateBinding)
	return contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID:         "discuss.late_binding",
		Message:    msg,
		Kind:       contextfrag.KindSystemPolicy,
		Slot:       contextfrag.SlotAfterCurrent,
		Priority:   90,
		CacheClass: contextfrag.CacheNever,
		Trust:      contextfrag.TrustSystem,
		Scope:      scope,
		Source:     discussContextSource,
		SourceID:   "late_binding",
		Collector:  discussContextCollectorName,
	})
}

type discussSourceEntry struct {
	kind  string
	time  int64
	index int
	rc    pipeline.RenderedSegment
	tr    pipeline.TurnResponseEntry
}

func sortedDiscussSourceEntries(rc pipeline.RenderedContext, trs []pipeline.TurnResponseEntry) []discussSourceEntry {
	entries := make([]discussSourceEntry, 0, len(rc)+len(trs))
	for i, seg := range rc {
		entries = append(entries, discussSourceEntry{
			kind:  "rc",
			time:  seg.ReceivedAtMs,
			index: i,
			rc:    seg,
		})
	}
	for i, tr := range trs {
		entries = append(entries, discussSourceEntry{
			kind:  "tr",
			time:  tr.RequestedAtMs,
			index: i,
			tr:    tr,
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].time != entries[j].time {
			return entries[i].time < entries[j].time
		}
		if entries[i].kind != entries[j].kind {
			return entries[i].kind == "rc"
		}
		return entries[i].index < entries[j].index
	})
	return entries
}

func discussRCFrag(seg pipeline.RenderedSegment, index int, scope contextfrag.Scope) (contextfrag.ContextFrag, bool) {
	text := discussRCText(seg)
	if text == "" {
		return contextfrag.ContextFrag{}, false
	}
	msg := sdk.UserMessage(text)
	return contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID:         fmt.Sprintf("discuss.rc.%03d", index),
		Message:    msg,
		Kind:       contextfrag.KindConversationEvent,
		Slot:       contextfrag.SlotHistory,
		Priority:   contextfrag.PriorityForMessage(msg),
		CacheClass: contextfrag.CacheNever,
		Trust:      contextfrag.TrustExternal,
		Scope:      discussSegmentScope(seg, scope),
		Source:     discussContextSource,
		SourceID:   fmt.Sprintf("rc.%03d", index),
		Collector:  discussContextCollectorName,
		Index:      index,
	}), true
}

// discussSegmentScope narrows the whole-turn scope to the segment's own
// message when the renderer supplied structured metadata; fixture segments
// without metadata keep the turn scope untouched.
func discussSegmentScope(seg pipeline.RenderedSegment, scope contextfrag.Scope) contextfrag.Scope {
	meta := seg.Meta
	if meta == nil && len(seg.ImageRefs) == 0 {
		return scope
	}
	out := scope
	extra := map[string]string{}
	if meta != nil {
		out.CurrentMessageID = strings.TrimSpace(meta.MessageID)
		out.ReplyToMessageID = strings.TrimSpace(meta.ReplyToMessageID)
		out.ReplySender = strings.TrimSpace(meta.ReplyToSender)
		out.ForwardMessageID = strings.TrimSpace(meta.ForwardMessageID)
		out.ForwardFromUserID = strings.TrimSpace(meta.ForwardFromUserID)
		out.ForwardFromConversationID = strings.TrimSpace(meta.ForwardFromConversationID)
		out.MentionsBot = seg.MentionsMe
		out.RepliesToBot = seg.RepliesToMe
		out.Attention = discussSegmentAttention(seg, out.ConversationType)
		if name := firstNonEmptyTrimmed(meta.SenderDisplayName, meta.SenderUsername, meta.SenderID); name != "" {
			out.DisplayName = name
		}
		if senderID := strings.TrimSpace(meta.SenderID); senderID != "" {
			extra["sender_id"] = senderID
		}
	}
	if len(seg.ImageRefs) > 0 {
		refs := make([]string, 0, len(seg.ImageRefs))
		for _, ref := range seg.ImageRefs {
			refs = append(refs, ref.ContentHash+":"+ref.Mime)
		}
		extra["image_refs"] = strings.Join(refs, ",")
	}
	if len(extra) > 0 {
		metadata := make(map[string]string, len(out.Metadata)+len(extra))
		for k, v := range out.Metadata {
			metadata[k] = v
		}
		for k, v := range extra {
			metadata[k] = v
		}
		out.Metadata = metadata
	}
	return out
}

func firstNonEmptyTrimmed(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// discussSegmentAttention shares the chat-side attention core
// (contextfrag.DeriveAttention) but prefers the segment's own conversation
// type over the turn's and buckets an unknown type as passive group history.
func discussSegmentAttention(seg pipeline.RenderedSegment, turnConversationType string) []contextfrag.AttentionReason {
	conversationType := turnConversationType
	if seg.Meta != nil && strings.TrimSpace(seg.Meta.ConversationType) != "" {
		conversationType = seg.Meta.ConversationType
	}
	return contextfrag.DeriveAttention(contextfrag.AttentionDerivation{
		ConversationType: conversationType,
		MentionsBot:      seg.MentionsMe,
		RepliesToBot:     seg.RepliesToMe,
		UnknownTypeKind:  "group",
	})
}

// discussRCText consumes the segment's already-rendered bytes; switch to
// pipeline.RenderMessageSegment when the first consumer needs a render target
// other than the pipeline's own output.
func discussRCText(seg pipeline.RenderedSegment) string {
	var out strings.Builder
	for _, piece := range seg.Content {
		if piece.Type != "text" {
			continue
		}
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(piece.Text)
	}
	return out.String()
}

func discussTRFrag(tr pipeline.TurnResponseEntry, index int, scope contextfrag.Scope) contextfrag.ContextFrag {
	msg := discussContextMessageToSDK(pipeline.ContextMessage{
		Role:       tr.Role,
		Content:    tr.Content,
		RawContent: tr.RawContent,
	})
	return contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID:         fmt.Sprintf("discuss.tr.%03d", index),
		Message:    msg,
		Kind:       contextfrag.KindConversationEvent,
		Slot:       contextfrag.SlotHistory,
		Priority:   contextfrag.PriorityForMessage(msg),
		CacheClass: contextfrag.CacheNever,
		Trust:      trustForDiscussRole(tr.Role),
		Scope:      scope,
		Source:     discussContextSource,
		SourceID:   fmt.Sprintf("tr.%03d", index),
		Collector:  discussContextCollectorName,
		Index:      index,
	})
}

func discussContextConfig(config any) (DiscussContextConfig, error) {
	return collectorConfig[DiscussContextConfig](config, "discuss_context config must be DiscussContextConfig")
}

func discussContextMessageToSDK(m pipeline.ContextMessage) sdk.Message {
	if len(m.RawContent) > 0 {
		raw, err := json.Marshal(struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}{
			Role:    m.Role,
			Content: m.RawContent,
		})
		if err == nil {
			var msg sdk.Message
			if json.Unmarshal(raw, &msg) == nil {
				return msg
			}
		}
	}
	switch m.Role {
	case "user":
		return sdk.UserMessage(m.Content)
	case "assistant":
		return sdk.AssistantMessage(m.Content)
	case "tool":
		return sdk.UserMessage(m.Content)
	default:
		return sdk.UserMessage(m.Content)
	}
}

func trustForDiscussRole(role string) contextfrag.TrustLevel {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "assistant", "tool":
		return contextfrag.TrustWorkspace
	default:
		return contextfrag.TrustExternal
	}
}
