// Package runconfig builds what one model request runs with: the model and its
// credentials, the system prompt, the context messages, the attachments and
// the workspace the run executes in. Building only reads; what a turn writes
// back or wires up after it has resolved belongs to the caller.
package runconfig

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/background"
	"github.com/felinics/memoh/internal/agent/botenv"
	"github.com/felinics/memoh/internal/agent/compact"
	"github.com/felinics/memoh/internal/agent/decide"
	"github.com/felinics/memoh/internal/agent/memory"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/transcript"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/providers"
	"github.com/felinics/memoh/internal/workdir"
)

// Queries is the part of the database the builder reads and writes.
type Queries interface {
	GetSessionByID(ctx context.Context, id pgtype.UUID) (sqlc.BotSession, error)
	GetLatestSessionModelID(ctx context.Context, sessionID pgtype.UUID) (pgtype.UUID, error)
	GetProviderByID(ctx context.Context, id pgtype.UUID) (sqlc.Provider, error)
	CompareAndSetSessionModelPreference(ctx context.Context, arg sqlc.CompareAndSetSessionModelPreferenceParams) (int64, error)
}

// Credentials resolves the credentials a provider's requests are signed with.
type Credentials interface {
	ResolveModelCredentials(ctx context.Context, provider sqlc.Provider) (providers.ModelCredentials, error)
}

// Deps is everything New needs. The first block is required; a builder
// without it is a configuration error that surfaces as the failure the missing
// piece causes. The second block may stay zero, which turns the feature it
// names off.
type Deps struct {
	// Required.

	// Logger receives the builder's own records.
	Logger *slog.Logger
	// Queries reads the session's model preference and the model catalog.
	Queries Queries
	// Credentials resolves provider credentials.
	Credentials Credentials
	// Models resolves model and provider configuration.
	Models *models.Service
	// Sessions reads session metadata.
	Sessions runreq.SessionService
	// Agent is the in-process model runtime, which supplies limits and the
	// workspace bridge.
	Agent *native.Agent
	// Bot supplies the bot's settings, identity and hooks.
	Bot *botenv.Env
	// Transcript assembles the history a run starts from.
	Transcript *transcript.Reader
	// Memory recalls what the bot remembers about the request.
	Memory *memory.Keeper
	// Compact judges and compacts the context before the first call.
	Compact *compact.Compactor
	// Decider builds the handler a tool approval is decided through.
	Decider *decide.Decider
	// BotPermissions decides whether an account may reach a workspace target.
	BotPermissions runreq.BotPermissionChecker
	// WorkspaceTargets resolves a request-scoped Computer.
	WorkspaceTargets WorkspaceTargetResolver

	// Optional.

	// Skills lists a bot's usable skills; without it requests carry none.
	Skills SkillLoader
	// Assets inlines attachment content for the gateway; without it
	// attachments keep their stored references.
	Assets GatewayAssetLoader
	// PlatformIdentities supplies connected platform accounts for the system
	// prompt; without it the prompt names none.
	PlatformIdentities PlatformIdentitySource
	// Workdirs resolves session workdir bindings; without it bindings are
	// ignored.
	Workdirs WorkdirResolver
	// Background supplies background task summaries; without it there are none.
	Background *background.Manager
	// HTTPClient carries model requests; without it a client that bounds
	// connection setup but leaves the stream's own silence to the idle
	// watchdog is built.
	HTTPClient *http.Client
}

// Builder builds the run configuration of a model request.
type Builder struct {
	logger      *slog.Logger
	queries     Queries
	credentials Credentials
	models      *models.Service
	sessions    runreq.SessionService
	agent       *native.Agent
	bot         *botenv.Env
	transcript  *transcript.Reader
	memory      *memory.Keeper
	compact     *compact.Compactor
	decider     *decide.Decider
	permissions runreq.BotPermissionChecker
	targets     WorkspaceTargetResolver
	skills      SkillLoader
	assets      GatewayAssetLoader
	identities  PlatformIdentitySource
	workdirs    workdirBindings
	background  *background.Manager
	httpClient  *http.Client
	animations  animationCache
}

// New returns a Builder. Absent optional dependencies become implementations
// that answer as an unconfigured feature does.
func New(d Deps) *Builder {
	b := &Builder{
		logger:      d.Logger,
		queries:     d.Queries,
		credentials: d.Credentials,
		models:      d.Models,
		sessions:    d.Sessions,
		agent:       d.Agent,
		bot:         d.Bot,
		transcript:  d.Transcript,
		memory:      d.Memory,
		compact:     d.Compact,
		decider:     d.Decider,
		permissions: d.BotPermissions,
		targets:     d.WorkspaceTargets,
		skills:      d.Skills,
		assets:      d.Assets,
		identities:  d.PlatformIdentities,
		background:  d.Background,
		httpClient:  d.HTTPClient,
	}
	if b.logger == nil {
		b.logger = slog.New(slog.DiscardHandler)
	}
	if b.skills == nil {
		b.skills = noSkills{}
	}
	if b.assets == nil {
		b.assets = noAssets{}
	}
	if b.identities == nil {
		b.identities = noIdentities{}
	}
	b.workdirs = noWorkdirs{}
	if d.Workdirs != nil && d.Sessions != nil {
		b.workdirs = sessionWorkdirs{sessions: d.Sessions, workdirs: d.Workdirs}
	}
	if b.httpClient == nil {
		b.httpClient = newStreamClient()
	}
	return b
}

// newStreamClient keeps connection setup bounded while the run's idle watchdog
// owns first-byte and between-event silence. A client-wide or response-header
// deadline would preempt that policy and turn one intentional timeout into
// repeated transport retries.
func newStreamClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}}
}

var errAssetsNotConfigured = errors.New("gateway asset loader not configured")

type noSkills struct{}

func (noSkills) LoadSkills(context.Context, string) ([]SkillEntry, error) { return nil, nil }

type noIdentities struct{}

func (noIdentities) ListPlatformIdentities(context.Context, string) ([]PlatformIdentity, error) {
	return nil, nil
}

// noAssets answers as a loader that is not configured does.
type noAssets struct{}

func (noAssets) OpenForGateway(context.Context, string, string) (io.ReadCloser, string, error) {
	return nil, "", errAssetsNotConfigured
}

func (noAssets) AccessPathForGateway(context.Context, string, string) (string, error) {
	return "", errAssetsNotConfigured
}

// noWorkdirs binds no session to a workdir.
type noWorkdirs struct{}

func (noWorkdirs) Binding(context.Context, string, string) (workdir.Resolved, bool, error) {
	return workdir.Resolved{}, false, nil
}

var _ = native.RunConfig{}
