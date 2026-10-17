package title

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/singleflight"

	"github.com/felinics/memoh/internal/accounts"
	messageevent "github.com/felinics/memoh/internal/chat/event"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	memprovider "github.com/felinics/memoh/internal/memory/adapters"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/providers"
)

// Store is the part of the database port the generator reads.
type Store interface {
	GetBotByID(ctx context.Context, id pgtype.UUID) (sqlc.GetBotByIDRow, error)
	ListSessionsByBotAndCreatedByUserPaged(ctx context.Context, arg sqlc.ListSessionsByBotAndCreatedByUserPagedParams) ([]sqlc.ListSessionsByBotAndCreatedByUserPagedRow, error)
	ListBotAppInstallations(ctx context.Context, botID pgtype.UUID) ([]sqlc.BotAppInstallation, error)
}

// Sessions reads a session and writes its title.
type Sessions interface {
	Get(ctx context.Context, sessionID string) (session.Thread, error)
	UpdateTitle(ctx context.Context, sessionID, title string) (session.Thread, error)
}

// Accounts resolves the account that owns a bot.
type Accounts interface {
	Get(ctx context.Context, userID string) (accounts.Account, error)
}

// ChatModels resolves a chat model and its provider by model id.
type ChatModels interface {
	FetchChatModel(ctx context.Context, modelID string) (models.GetResponse, sqlc.Provider, error)
}

// Memory finds the memory provider a bot is configured with, or nil.
type Memory interface {
	Provider(ctx context.Context, botID string) memprovider.Provider
}

// Credentials resolves the credentials a provider's requests are signed with.
type Credentials interface {
	ResolveModelCredentials(ctx context.Context, provider sqlc.Provider) (providers.ModelCredentials, error)
}

// Deps is everything New needs.
type Deps struct {
	// Logger receives the generator's own records.
	Logger *slog.Logger
	// Store, Sessions, Accounts, Models, Memory and Credentials are required.
	Store       Store
	Sessions    Sessions
	Accounts    Accounts
	Models      ChatModels
	Memory      Memory
	Credentials Credentials
	// Events broadcasts the title updates; without it none are sent.
	Events messageevent.Publisher
}

// Generator produces session-level one-shot text: titles and starter prompts.
type Generator struct {
	logger      *slog.Logger
	store       Store
	sessions    Sessions
	accounts    Accounts
	models      ChatModels
	memory      Memory
	credentials Credentials
	events      messageevent.Publisher
	// starterFlights shares one generation among concurrent calls for the same
	// material.
	starterFlights singleflight.Group
}

// New returns a Generator.
func New(d Deps) *Generator {
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	events := d.Events
	if events == nil {
		events = noEvents{}
	}
	return &Generator{
		logger:      logger,
		store:       d.Store,
		sessions:    d.Sessions,
		accounts:    d.Accounts,
		models:      d.Models,
		memory:      d.Memory,
		credentials: d.Credentials,
		events:      events,
	}
}

// noEvents is the publisher of a deployment that broadcasts nothing.
type noEvents struct{}

func (noEvents) Publish(messageevent.Event) {}
