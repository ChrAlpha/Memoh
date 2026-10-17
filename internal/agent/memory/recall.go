package memory

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/felinics/memoh/internal/agent/botenv"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/hooks"
	memprovider "github.com/felinics/memoh/internal/memory/adapters"
)

// Recalled is what memory contributes to a turn: the provider's context, the
// text hooks appended to it, the message that carries both, and a trace of how
// it was found.
type Recalled struct {
	MemoryText string
	HookText   string
	Message    *runreq.ModelMessage
	Trace      *contextfrag.MemoryRecallTrace
}

// Recall loads the context the bot's memory provider holds for the request.
// A bot without a provider, and a request without a query, recall nothing.
func (k *Keeper) Recall(ctx context.Context, req runreq.ChatRequest) Recalled {
	builtQuery := k.buildQuery(ctx, req)
	if strings.TrimSpace(builtQuery.Query) == "" {
		return Recalled{}
	}
	providerID, p := k.providers.find(ctx, req.BotID)
	if p == nil {
		return Recalled{}
	}
	cacheKey := k.cacheKey(ctx, req, providerID, p, builtQuery.Query)

	before, err := k.bot.RunChatHook(ctx, req, hooks.EventBeforeMemorySearch, func(hreq *hooks.Request) {
		hreq.Memory = map[string]any{
			"scope":                 "before_chat",
			"query":                 builtQuery.Query,
			"visible_query":         strings.TrimSpace(req.Query),
			"query_source":          builtQuery.Source,
			"query_recent_messages": builtQuery.RecentMessages,
			"query_truncated":       builtQuery.Truncated,
		}
	})
	if err != nil {
		k.bot.LogHookWarn(hooks.EventBeforeMemorySearch, req.BotID, req.ThreadID, err)
		if before.Decision == hooks.DecisionDeny {
			return Recalled{Trace: recallTrace(cacheKey, builtQuery, nil, "bypass", "hook_denied")}
		}
	}

	if cached, ok := k.cache.Get(cacheKey); ok {
		result := &memprovider.BeforeChatResult{
			ContextText:    cached.ContextText,
			RetrievalMode:  cached.RetrievalMode,
			FallbackReason: cached.FallbackReason,
			ResultCount:    cached.ResultCount,
			ResultRefs:     cached.ResultRefs,
		}
		return k.recalledFromResult(ctx, req, builtQuery, cacheKey, result, "fresh", "")
	}

	searchCtx, cancel := context.WithTimeout(ctx, k.searchTimeout)
	result, err := p.OnBeforeChat(searchCtx, memprovider.BeforeChatRequest{
		Query:  builtQuery.Query,
		BotID:  req.BotID,
		ChatID: req.ChatID,
	})
	cancel()

	if err != nil {
		fallbackReason := "provider_error"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(searchCtx.Err(), context.DeadlineExceeded) {
			fallbackReason = "timeout"
		}
		k.logger.WarnContext(ctx, "memory provider OnBeforeChat failed",
			slog.String("bot_id", req.BotID),
			slog.String("fallback_reason", fallbackReason),
			slog.Any("error", err),
		)
		if cached, cacheState, ok := k.cache.GetFreshOrStale(cacheKey); ok {
			result := &memprovider.BeforeChatResult{
				ContextText:    cached.ContextText,
				RetrievalMode:  cached.RetrievalMode,
				FallbackReason: runreq.FirstNonEmpty(fallbackReason, cached.FallbackReason),
				ResultCount:    cached.ResultCount,
				ResultRefs:     cached.ResultRefs,
			}
			return k.recalledFromResult(ctx, req, builtQuery, cacheKey, result, string(cacheState), fallbackReason)
		}
		return k.recalledFromResult(ctx, req, builtQuery, cacheKey, nil, "miss", fallbackReason)
	}

	if result == nil || strings.TrimSpace(result.ContextText) == "" {
		return k.recalledFromResult(ctx, req, builtQuery, cacheKey, nil, "miss", "empty_result")
	}

	k.cache.Set(cacheKey, memprovider.MemoryContextCacheValue{
		ContextText:    result.ContextText,
		RetrievalMode:  result.RetrievalMode,
		FallbackReason: result.FallbackReason,
		ResultCount:    result.ResultCount,
		ResultRefs:     result.ResultRefs,
	})
	return k.recalledFromResult(ctx, req, builtQuery, cacheKey, result, "miss", strings.TrimSpace(result.FallbackReason))
}

func (k *Keeper) recalledFromResult(ctx context.Context, req runreq.ChatRequest, builtQuery query, cacheKey memprovider.MemoryContextCacheKey, result *memprovider.BeforeChatResult, cacheState, fallbackReason string) Recalled {
	contextText := ""
	if result != nil {
		contextText = strings.TrimSpace(result.ContextText)
	}
	trace := recallTrace(cacheKey, builtQuery, result, cacheState, fallbackReason)

	after, err := k.bot.RunChatHook(ctx, req, hooks.EventAfterMemorySearch, func(hreq *hooks.Request) {
		hreq.Memory = map[string]any{
			"scope":                 "before_chat",
			"query":                 builtQuery.Query,
			"visible_query":         strings.TrimSpace(req.Query),
			"query_source":          builtQuery.Source,
			"query_recent_messages": builtQuery.RecentMessages,
			"query_truncated":       builtQuery.Truncated,
			"result_count":          trace.Result.Count,
			"context_bytes":         trace.Result.ContextBytes,
			"cache_hit":             cacheState == "fresh" || cacheState == "stale",
			"cache_state":           cacheState,
			"retrieval_mode":        trace.RetrievalMode,
			"fallback_reason":       trace.FallbackReason,
		}
	})
	if err != nil {
		k.bot.LogHookWarn(hooks.EventAfterMemorySearch, req.BotID, req.ThreadID, err)
	}

	load := materialize(contextText, after.AppendContext)
	load.Trace = trace
	return load
}

func materialize(memoryText, hookText string) Recalled {
	memoryText = strings.TrimSpace(memoryText)
	hookText = botenv.FormatHookContext(hooks.EventAfterMemorySearch, hookText)
	combined := memoryText
	if hookText != "" {
		if combined != "" {
			combined += "\n\n"
		}
		combined += hookText
	}
	load := Recalled{MemoryText: memoryText, HookText: hookText}
	if combined != "" {
		load.Message = &runreq.ModelMessage{Role: "user", Content: runreq.NewTextContent(combined)}
	}
	return load
}

func recallTrace(cacheKey memprovider.MemoryContextCacheKey, builtQuery query, result *memprovider.BeforeChatResult, cacheState, fallbackReason string) *contextfrag.MemoryRecallTrace {
	contextText := ""
	resultCount := 0
	retrievalMode := ""
	var resultRefs []string
	if result != nil {
		contextText = strings.TrimSpace(result.ContextText)
		resultCount = max(result.ResultCount, 0)
		resultRefs = append([]string(nil), result.ResultRefs...)
		retrievalMode = strings.TrimSpace(result.RetrievalMode)
		if fallbackReason == "" {
			fallbackReason = strings.TrimSpace(result.FallbackReason)
		}
	}
	return &contextfrag.MemoryRecallTrace{
		ProviderID:     strings.TrimSpace(cacheKey.ProviderID),
		MemoryVersion:  strings.TrimSpace(cacheKey.MemoryVersion),
		CacheState:     strings.TrimSpace(cacheState),
		RetrievalMode:  retrievalMode,
		FallbackReason: strings.TrimSpace(fallbackReason),
		Query: contextfrag.MemoryRecallQueryTrace{
			Source:         builtQuery.Source,
			RecentMessages: builtQuery.RecentMessages,
			Truncated:      builtQuery.Truncated,
		},
		Result: contextfrag.MemoryRecallResultTrace{
			Count:        resultCount,
			Refs:         resultRefs,
			ContextBytes: len(contextText),
		},
	}
}

func (*Keeper) cacheKey(ctx context.Context, req runreq.ChatRequest, providerID string, p memprovider.Provider, text string) memprovider.MemoryContextCacheKey {
	memoryVersion := ""
	if versioned, ok := p.(memprovider.MemoryVersionProvider); ok {
		memoryVersion = versioned.MemoryVersion(ctx, req.BotID)
	}
	return memprovider.MemoryContextCacheKey{
		BotID:         strings.TrimSpace(req.BotID),
		ChatID:        strings.TrimSpace(req.ChatID),
		ProviderID:    strings.TrimSpace(providerID),
		QueryHash:     memprovider.MemoryContextQueryHash(text),
		MemoryVersion: strings.TrimSpace(memoryVersion),
	}
}
