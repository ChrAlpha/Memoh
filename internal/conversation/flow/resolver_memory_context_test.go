package flow

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/memohai/memoh/internal/conversation"
	"github.com/memohai/memoh/internal/hooks"
	memprovider "github.com/memohai/memoh/internal/memory/adapters"
	"github.com/memohai/memoh/internal/settings"
)

func TestLoadMemoryContextMessage_NoProvider(t *testing.T) {
	resolver := &Resolver{
		logger: slog.Default(),
	}
	got := resolver.loadMemoryContext(context.Background(), conversation.ChatRequest{
		Query:  "hello",
		BotID:  "bot-1",
		ChatID: "chat-1",
	})
	if got.MemoryText != "" || got.HookText != "" {
		t.Fatalf("expected empty memory context when no provider is configured: %#v", got)
	}
}

func TestLoadMemoryContextMessageSkipsEmptyQuery(t *testing.T) {
	t.Parallel()

	registry := memprovider.NewRegistry(slog.New(slog.DiscardHandler))
	registry.Register(storeRoundMemoryProviderID, &storeRoundMemoryProvider{afterChat: make(chan memprovider.AfterChatRequest, 1)})
	resolver := &Resolver{
		memoryRegistry:  registry,
		settingsService: settings.NewService(slog.New(slog.DiscardHandler), &storeRoundSettingsQueries{}, nil, nil),
		logger:          slog.New(slog.DiscardHandler),
	}
	got := resolver.loadMemoryContext(context.Background(), conversation.ChatRequest{
		Query:      "",
		ModelQuery: "The user activated the following skill for this turn without an additional prompt: alpha.",
		BotID:      storeRoundBotID,
		ChatID:     "chat-1",
	})
	if got.MemoryText != "" || got.HookText != "" {
		t.Fatalf("expected empty memory context for empty visible query, got %#v", got)
	}
}

func TestLoadMemoryContextMessageUsesStaleCacheOnTimeout(t *testing.T) {
	t.Parallel()

	now := time.Unix(100, 0)
	provider := &slowBeforeChatProvider{
		result: &memprovider.BeforeChatResult{
			ContextText:   "<memory-context>cached memory</memory-context>",
			RetrievalMode: "graph",
		},
	}
	registry := memprovider.NewRegistry(slog.New(slog.DiscardHandler))
	registry.Register(storeRoundMemoryProviderID, provider)
	resolver := &Resolver{
		memoryRegistry:      registry,
		settingsService:     settings.NewService(slog.New(slog.DiscardHandler), &storeRoundSettingsQueries{}, nil, nil),
		logger:              slog.New(slog.DiscardHandler),
		memorySearchTimeout: 5 * time.Millisecond,
		memoryContextCache: memprovider.NewMemoryContextCache(memprovider.MemoryContextCacheConfig{
			TTL:      time.Millisecond,
			StaleTTL: time.Minute,
			Now: func() time.Time {
				return now
			},
		}),
	}
	req := conversation.ChatRequest{
		Query:  "tea",
		BotID:  storeRoundBotID,
		ChatID: "chat-1",
	}

	first := resolver.loadMemoryContext(context.Background(), req)
	if !strings.Contains(first.MemoryText, "cached memory") {
		t.Fatalf("expected first memory context, got %#v", first)
	}

	now = now.Add(2 * time.Millisecond)
	provider.waitForContext = true
	second := resolver.loadMemoryContext(context.Background(), req)
	if !strings.Contains(second.MemoryText, "cached memory") {
		t.Fatalf("expected stale memory context after timeout, got %#v", second)
	}
	if provider.calls < 2 {
		t.Fatalf("expected provider to be called again after fresh TTL expired, got %d calls", provider.calls)
	}
}

func TestLoadMemoryContextMessageInvalidatesCacheWhenMemoryVersionChanges(t *testing.T) {
	t.Parallel()

	provider := &versionedBeforeChatProvider{
		slowBeforeChatProvider: slowBeforeChatProvider{result: &memprovider.BeforeChatResult{ContextText: "memory v1"}},
		version:                "v1",
	}
	registry := memprovider.NewRegistry(slog.New(slog.DiscardHandler))
	registry.Register(storeRoundMemoryProviderID, provider)
	resolver := &Resolver{
		memoryRegistry:  registry,
		settingsService: settings.NewService(slog.New(slog.DiscardHandler), &storeRoundSettingsQueries{}, nil, nil),
		logger:          slog.New(slog.DiscardHandler),
	}
	req := conversation.ChatRequest{Query: "tea", BotID: storeRoundBotID, ChatID: "chat-1"}

	first := resolver.loadMemoryContext(context.Background(), req)
	if first.MemoryText != "memory v1" {
		t.Fatalf("first memory context = %#v, want v1", first)
	}
	provider.version = "v2"
	provider.result = &memprovider.BeforeChatResult{ContextText: "memory v2"}
	second := resolver.loadMemoryContext(context.Background(), req)
	if second.MemoryText != "memory v2" {
		t.Fatalf("second memory context = %#v, want v2", second)
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d, want cache miss after version change", provider.calls)
	}
}

func TestMaterializeMemoryContextSeparatesHookOutput(t *testing.T) {
	t.Parallel()

	got := materializeMemoryContext("remembered fact", "plugin guidance")
	if got.MemoryText != "remembered fact" {
		t.Fatalf("memory text = %q, want provider result only", got.MemoryText)
	}
	if got.HookText != "[Hook Context: "+hooks.EventAfterMemorySearch+"]\nplugin guidance" {
		t.Fatalf("hook text = %q, want separately attributed hook context", got.HookText)
	}
	if strings.Contains(got.MemoryText, "plugin guidance") {
		t.Fatalf("hook output leaked into memory text: %q", got.MemoryText)
	}
}

type slowBeforeChatProvider struct {
	memprovider.Provider
	result         *memprovider.BeforeChatResult
	waitForContext bool
	calls          int
}

func (*slowBeforeChatProvider) Type() string {
	return "test"
}

func (p *slowBeforeChatProvider) OnBeforeChat(ctx context.Context, _ memprovider.BeforeChatRequest) (*memprovider.BeforeChatResult, error) {
	p.calls++
	if p.waitForContext {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.result, nil
}

type versionedBeforeChatProvider struct {
	slowBeforeChatProvider
	version string
}

func (p *versionedBeforeChatProvider) MemoryVersion(context.Context, string) string {
	return p.version
}
