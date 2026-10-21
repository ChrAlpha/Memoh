package memory

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/felinics/memoh/internal/agent/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/errlog"
	"github.com/felinics/memoh/internal/errs"
	"github.com/felinics/memoh/internal/hooks"
	"github.com/felinics/memoh/internal/job"
	memprovider "github.com/felinics/memoh/internal/memory/adapters"
	"github.com/felinics/memoh/internal/memory/storefs"
	"github.com/felinics/memoh/internal/runtimekind"
)

// RoundPersisted starts a background unit that hands a persisted round to the
// bot's memory provider.
func (k *Keeper) RoundPersisted(ctx context.Context, req runreq.ChatRequest, persisted []messagepkg.Message) {
	botID := strings.TrimSpace(req.BotID)
	if botID == "" || !k.providers.enabled() {
		return
	}
	job.Go(ctx, k.logger, "agent.memory", job.Options{}, func(ctx context.Context) error {
		return k.write(ctx, req, botID, persisted)
	}, slog.String("bot_id", botID), slog.String("session_id", req.ThreadID))
}

// write is the body of one memory unit.
func (k *Keeper) write(ctx context.Context, req runreq.ChatRequest, botID string, persisted []messagepkg.Message) error {
	requestIncluded := false
	for _, message := range persisted {
		if strings.EqualFold(strings.TrimSpace(message.Role), "user") && !messagepkg.IsInternalFeedback(message.Metadata) &&
			((req.PersistedUserMessageID != "" && message.ID == req.PersistedUserMessageID) || (req.TurnID != "" && message.TurnID == req.TurnID)) {
			requestIncluded = true
			break
		}
	}
	if (req.UserMessagePersisted || req.ReusePersistedUserMessage) && !requestIncluded {
		userMessage, err := k.messages.GetByIDBySession(ctx, req.ThreadID, req.PersistedUserMessageID)
		if err != nil {
			// The round is still written, without its user message.
			dropped := errs.Wrap(err, "load persisted user message for memory",
				slog.String("session_id", req.ThreadID),
				slog.String("message_id", req.PersistedUserMessageID),
			)
			result := errlog.Event(ctx, "agent.memory", dropped, errlog.Options{})
			k.logger.LogAttrs(ctx, result.Level, "load persisted user message for memory failed", result.Attrs()...)
		} else {
			persisted = append([]messagepkg.Message{userMessage}, persisted...)
		}
	}
	memMsgs := providerMessages(persisted)
	if len(memMsgs) == 0 {
		return nil
	}

	p := k.Provider(ctx, botID)
	if p == nil {
		job.Annotate(ctx, slog.String("skipped", "no_provider"))
		return nil
	}
	before, err := k.bot.RunChatHook(ctx, req, hooks.EventBeforeMemoryWrite, func(hreq *hooks.Request) {
		hreq.Memory = map[string]any{
			"scope":         "after_chat",
			"message_count": len(memMsgs),
		}
	})
	if err != nil {
		k.bot.LogHookWarn(hooks.EventBeforeMemoryWrite, botID, req.ThreadID, err)
		if before.Decision == hooks.DecisionDeny {
			job.Annotate(ctx, slog.String("skipped", "hook_denied"))
			return nil
		}
	}
	_, tzLoc := k.bot.ResolveTimezone(ctx, req.BotID, req.UserID)
	// A post-turn write belongs to a conversation that is still warm: it may
	// leave MEMORY.md for Overview.Refresh to rewrite.
	if err := p.OnAfterChat(storefs.WithConversationWrite(ctx), memprovider.AfterChatRequest{
		BotID:             botID,
		Messages:          memMsgs,
		UserID:            strings.TrimSpace(req.UserID),
		ChannelIdentityID: strings.TrimSpace(req.SourceChannelIdentityID),
		DisplayName:       k.bot.ResolveDisplayName(ctx, req),
		TimezoneLocation:  tzLoc,
		SkipFormation:     runtimekind.IsExternal(req.RuntimeType),
	}); err != nil {
		return errs.Wrap(err, "memory provider after chat")
	}
	_, _ = k.bot.RunChatHook(ctx, req, hooks.EventMemoryExtracted, func(hreq *hooks.Request) {
		hreq.Memory = map[string]any{
			"scope":         "after_chat",
			"message_count": len(memMsgs),
		}
	})
	if _, err := k.bot.RunChatHook(ctx, req, hooks.EventAfterMemoryWrite, func(hreq *hooks.Request) {
		hreq.Memory = map[string]any{
			"scope":         "after_chat",
			"message_count": len(memMsgs),
		}
	}); err != nil {
		k.bot.LogHookWarn(hooks.EventAfterMemoryWrite, botID, req.ThreadID, err)
	}
	return nil
}

func providerMessages(persisted []messagepkg.Message) []memprovider.Message {
	out := make([]memprovider.Message, 0, len(persisted))
	for _, stored := range persisted {
		var msg runreq.ModelMessage
		if err := json.Unmarshal(stored.Content, &msg); err != nil {
			continue
		}
		text := strings.TrimSpace(msg.TextContent())
		if text == "" {
			continue
		}
		sessionID := strings.TrimSpace(stored.SessionID)
		ref := memprovider.EncodeSourceRef(sessionID, stored.ID)
		if _, _, ok := memprovider.ParseScopedSourceRef(ref); !ok {
			continue
		}
		role := strings.TrimSpace(stored.Role)
		if role == "" {
			role = strings.TrimSpace(msg.Role)
		}
		out = append(out, memprovider.Message{Role: role, Content: text, SourceMessageID: ref})
	}
	return out
}
