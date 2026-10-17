// Package memory recalls what a bot's memory provider knows about the turn
// that is about to run, and hands the provider the rounds worth remembering.
package memory

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/felinics/memoh/internal/agent/botenv"
	"github.com/felinics/memoh/internal/agent/transcript"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	memprovider "github.com/felinics/memoh/internal/memory/adapters"
	"github.com/felinics/memoh/internal/settings"
)

const defaultSearchTimeout = 1200 * time.Millisecond

// Settings is the part of the bot settings store that names a bot's memory
// provider.
type Settings interface {
	GetBot(ctx context.Context, botID string) (settings.Settings, error)
}

// Messages reads the persisted user message a round answers.
type Messages interface {
	GetByIDBySession(ctx context.Context, sessionID, messageID string) (messagepkg.Message, error)
}

// Deps is everything New needs.
type Deps struct {
	// Logger receives the keeper's own records.
	Logger *slog.Logger
	// Settings names a bot's memory provider.
	Settings Settings
	// Messages reads persisted messages.
	Messages Messages
	// Bot runs the memory hooks and resolves who a round belongs to.
	Bot *botenv.Env
	// Transcript supplies the recent history a query is built from.
	Transcript *transcript.Reader
	// Registry holds the memory providers; without it a turn recalls nothing
	// and no round is handed over.
	Registry *memprovider.Registry
	// SearchTimeout bounds one provider search; zero keeps the default.
	SearchTimeout time.Duration
	// Cache holds recalled context between turns; nil builds the default.
	Cache *memprovider.MemoryContextCache
}

// Keeper recalls and stores memory.
type Keeper struct {
	logger        *slog.Logger
	messages      Messages
	bot           *botenv.Env
	transcript    *transcript.Reader
	providers     providers
	searchTimeout time.Duration
	cache         *memprovider.MemoryContextCache
}

// New returns a Keeper. Without a registry it is built with stand-ins that
// find no provider.
func New(d Deps) *Keeper {
	k := &Keeper{
		logger:        d.Logger,
		messages:      d.Messages,
		bot:           d.Bot,
		transcript:    d.Transcript,
		providers:     noProviders{},
		searchTimeout: d.SearchTimeout,
		cache:         d.Cache,
	}
	if d.Registry != nil {
		k.providers = registryProviders{logger: d.Logger, settings: d.Settings, registry: d.Registry}
	}
	if k.searchTimeout <= 0 {
		k.searchTimeout = defaultSearchTimeout
	}
	if k.cache == nil {
		k.cache = memprovider.NewMemoryContextCache(memprovider.MemoryContextCacheConfig{
			TTL:        time.Minute,
			StaleTTL:   5 * time.Minute,
			MaxEntries: 256,
		})
	}
	return k
}

// providers finds the memory provider a bot is configured with.
type providers interface {
	// find returns the provider's id and the provider, or nothing when the bot
	// has none.
	find(ctx context.Context, botID string) (string, memprovider.Provider)
	// enabled says whether memory is configured at all.
	enabled() bool
}

type registryProviders struct {
	logger   *slog.Logger
	settings Settings
	registry *memprovider.Registry
}

func (registryProviders) enabled() bool { return true }

func (r registryProviders) find(ctx context.Context, botID string) (string, memprovider.Provider) {
	botSettings, err := r.settings.GetBot(ctx, botID)
	if err != nil {
		return "", nil
	}
	providerID := strings.TrimSpace(botSettings.MemoryProviderID)
	if providerID == "" {
		return "", nil
	}
	p, err := r.registry.Get(ctx, providerID)
	if err != nil {
		r.logger.WarnContext(ctx, "memory provider lookup failed", slog.String("provider_id", providerID), slog.Any("error", err))
		return "", nil
	}
	return providerID, p
}

type noProviders struct{}

func (noProviders) enabled() bool { return false }

func (noProviders) find(context.Context, string) (string, memprovider.Provider) { return "", nil }

// Provider returns the memory provider the bot is configured with, or nil.
func (k *Keeper) Provider(ctx context.Context, botID string) memprovider.Provider {
	_, p := k.providers.find(ctx, botID)
	return p
}
