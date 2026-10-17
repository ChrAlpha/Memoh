package runconfig

import (
	"context"
	"io"

	"github.com/felinics/memoh/internal/workspace"
)

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

// gatewayAssetLoader resolves content_hash references to binary payloads for gateway dispatch.
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

type WorkspaceTargetResolver interface {
	ResolveWorkspaceTarget(ctx context.Context, botID, targetID string) (workspace.ResolvedWorkspaceTarget, error)
}
