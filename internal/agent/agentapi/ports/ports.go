// Package ports declares what the composition root supplies to agent.New in
// agent.Deps: the interfaces and value types of the dependencies the agent does
// not own. They live apart from the agentapi root because they name heavier
// packages (media, workspace, workdir, chat/thread) that the contract types
// must not drag along.
package ports

import (
	"context"
	"io"

	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/media"
	"github.com/felinics/memoh/internal/workdir"
	"github.com/felinics/memoh/internal/workspace"
)

// BotPermissionChecker answers whether an account holds a permission on a bot.
type BotPermissionChecker interface {
	HasBotPermission(ctx context.Context, botID, accountID, permission string) (bool, error)
}

// SessionService is the interface the agent uses for session metadata updates.
type SessionService interface {
	Get(ctx context.Context, sessionID string) (session.Thread, error)
	UpdateTitle(ctx context.Context, sessionID, title string) (session.Thread, error)
	UpdateMetadata(ctx context.Context, sessionID string, metadata map[string]any) (session.Thread, error)
	MergeRuntimeMetadata(ctx context.Context, sessionID, runtimeType string, delta map[string]any) (session.Thread, error)
}

// SkillEntry represents a skill loaded from the container.
type SkillEntry struct {
	Name        string
	Description string
	Content     string
	Path        string
	Metadata    map[string]any
}

// SkillLoader loads skills for a given bot from its container.
type SkillLoader interface {
	LoadSkills(ctx context.Context, botID string) ([]SkillEntry, error)
}

// GatewayAssetLoader resolves content_hash references to binary payloads for gateway dispatch.
type GatewayAssetLoader interface {
	OpenForGateway(ctx context.Context, botID, contentHash string) (reader io.ReadCloser, mime string, err error)
	AccessPathForGateway(ctx context.Context, botID, contentHash string) (string, error)
}

// PlatformIdentity is the Agent-owned projection of a connected platform
// account used while assembling the system prompt.
type PlatformIdentity struct {
	ID               string
	Platform         string
	ExternalIdentity string
	SelfIdentity     map[string]any
}

// PlatformIdentitySource supplies connected platform identities without
// exposing Channel configuration types to the application layer.
type PlatformIdentitySource interface {
	ListPlatformIdentities(ctx context.Context, botID string) ([]PlatformIdentity, error)
}

// WorkspaceTargetResolver resolves a request-scoped Computer.
type WorkspaceTargetResolver interface {
	ResolveWorkspaceTarget(ctx context.Context, botID, targetID string) (workspace.ResolvedWorkspaceTarget, error)
}

// WorkdirResolver is the slice of *workdir.Service the agent needs:
// resolving a stored workdir binding to its target and directory.
type WorkdirResolver interface {
	ResolveForSession(ctx context.Context, botID, workdirID string) (workdir.Resolved, error)
}

// OutputMediaStore ingests the files an agent's events point at, so the event
// a subscriber receives names an asset it can fetch instead of a path only the
// workspace knows.
type OutputMediaStore interface {
	IngestContainerFile(ctx context.Context, botID, path string) (media.Asset, error)
	Ingest(ctx context.Context, in media.IngestInput) (media.Asset, error)
}

// OutputSpeech synthesizes the audio a speech event asks for. The composition
// root adapts the audio service and the bot's configured speech model onto it.
type OutputSpeech interface {
	SpeechModelID(ctx context.Context, botID string) (string, error)
	Synthesize(ctx context.Context, modelID, text string) (audio []byte, contentType string, err error)
}

// WorkspaceReadiness reports whether the workspace a run would start on can be
// reached; the error says why not. targetID empty is the session's default.
type WorkspaceReadiness func(ctx context.Context, botID, targetID string) error

// TeamScopePage contains opaque team IDs, never cross-team rows. Hosted
// deployments can reuse their runtime reaper's scope catalog.
type TeamScopePage struct {
	Scopes     []string
	NextCursor string
	Complete   bool
}

// TeamScopes answers which teams this process serves and binds each database
// operation to one of them. Session resume and the discuss sweep both
// enumerate through it. Implementations must enumerate through a restricted
// catalog, then use ordinary team-bound credentials for run queries,
// authorization and execution.
type TeamScopes interface {
	CurrentScope(context.Context) string
	ListScopePage(context.Context, string, int32) (TeamScopePage, error)
	BindScope(context.Context, string) context.Context
}
