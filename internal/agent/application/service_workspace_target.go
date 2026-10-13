package application

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/bots"
	"github.com/felinics/memoh/internal/workdir"
	"github.com/felinics/memoh/internal/workspace"
)

var ErrExternalAgentWorkspaceTargetUnsupported = errors.New("workspace_target_id is not supported for external agent sessions")

// ValidateWorkspaceTarget validates a user-selected Computer without changing
// the Bot's Primary target. It is used by handlers before creating a session.
func (s *Service) ValidateWorkspaceTarget(ctx context.Context, botID, targetID string) error {
	targetID = strings.TrimSpace(targetID)
	if targetID == "" {
		return nil
	}
	if s == nil || s.workspaceTargets == nil {
		return errors.New("workspace target resolver not configured")
	}
	_, err := s.workspaceTargets.ResolveWorkspaceTarget(ctx, strings.TrimSpace(botID), targetID)
	return err
}

// workspacePrincipal is the end user on whose behalf a run reaches a
// workspace target (barred class D: the user who started the run). A nil
// principal means the entry has no end user to authorize: scheduled runs act
// for the bot's owner, who holds every permission, and discuss, decision
// continuations and runtime controls only continue a session whose target was
// authorized when it was chosen. Those entries skip the workspace_read check.
// A non-nil principal with an empty UserID is an unidentified caller and is
// rejected whenever the check applies.
type workspacePrincipal struct {
	UserID string
}

// workspaceInput names everything resolveWorkspace needs.
type workspaceInput struct {
	BotID             string
	SessionID         string
	RequestedTargetID string
	Principal         *workspacePrincipal
	// BestEffort lets a run that selected no target and has none pinned
	// proceed when the default target cannot be resolved right now. Chat
	// entries leave it off so an unreachable workspace fails the turn up front.
	BestEffort bool
}

// resolvedWorkspace is the workspace a run executes in.
type resolvedWorkspace struct {
	// TargetID is the effective target; empty when no resolver is configured
	// and nothing selected one.
	TargetID string
	// Target is the resolved snapshot, nil when resolution was skipped.
	Target *WorkspaceTarget
	// WorkdirPath is the session's bound working directory, if any.
	WorkdirPath string
}

// resolveWorkspace is the single place a run's workspace target is decided:
// it applies the session's workdir binding, authorizes reaching a remote
// computer, resolves the target and pins it on the returned context.
func (s *Service) resolveWorkspace(ctx context.Context, in workspaceInput) (context.Context, resolvedWorkspace, error) {
	out := resolvedWorkspace{TargetID: strings.TrimSpace(in.RequestedTargetID)}
	bound, hasWorkdir, err := s.resolveSessionWorkdirBinding(ctx, in.BotID, in.SessionID)
	if err != nil {
		return ctx, out, err
	}
	enforceSelection := out.TargetID != ""
	if hasWorkdir {
		// The workdir pins the target for the session's whole life. An
		// explicit different target is rejected loudly — silently ignoring
		// it would let the user believe the switch took effect.
		if out.TargetID != "" && out.TargetID != bound.TargetID {
			return ctx, out, ErrWorkspaceTargetWorkdirConflict
		}
		out.TargetID = bound.TargetID
		out.WorkdirPath = bound.WorkDir
		// Reaching a remote computer is a permission boundary whether the
		// target comes from the request or from the workdir binding. A
		// native workdir adds no capability beyond the default workspace,
		// so it does not demand workspace_read just to chat.
		if bound.Kind == workdir.TargetKindRemote {
			enforceSelection = true
		}
	}
	if enforceSelection && in.Principal != nil {
		if strings.TrimSpace(in.Principal.UserID) == "" {
			return ctx, out, errors.New("user id is required to select a computer")
		}
		if s == nil || s.botPermissions == nil {
			return ctx, out, errors.New("workspace target permission checker not configured")
		}
		allowed, err := s.botPermissions.HasBotPermission(ctx, in.BotID, in.Principal.UserID, bots.PermissionWorkspaceRead)
		if err != nil {
			return ctx, out, fmt.Errorf("check workspace target permission: %w", err)
		}
		if !allowed {
			return ctx, out, errors.New("workspace_read permission is required to select a computer")
		}
	}
	if s == nil || s.workspaceTargets == nil {
		if out.TargetID != "" {
			return ctx, out, errors.New("workspace target resolver not configured")
		}
		return ctx, out, nil
	}
	if out.TargetID != "" {
		// Pin first so the resolution below, and its error path, run against
		// the chosen target.
		ctx = workspace.WithWorkspaceTarget(ctx, out.TargetID)
	}
	resolved, err := s.workspaceTargets.ResolveWorkspaceTarget(ctx, in.BotID, out.TargetID)
	if err != nil {
		if in.BestEffort && out.TargetID == "" && workspace.WorkspaceTargetFromContext(ctx) == "" {
			return ctx, out, nil
		}
		return ctx, out, err
	}
	out.Target = &WorkspaceTarget{
		TargetID: strings.TrimSpace(resolved.TargetID),
		Kind:     strings.TrimSpace(resolved.Kind),
		Name:     strings.TrimSpace(resolved.Name),
	}
	out.TargetID = out.Target.TargetID
	return workspace.WithWorkspaceTarget(ctx, out.TargetID), out, nil
}

// bindRequestWorkspace resolves the workspace for a request and records the
// effective target on it, so the user turn and every later row carry it.
func (s *Service) bindRequestWorkspace(ctx context.Context, req ChatRequest, principal *workspacePrincipal, bestEffort bool) (context.Context, ChatRequest, error) {
	ctx, ws, err := s.resolveWorkspace(ctx, workspaceInput{
		BotID:             req.BotID,
		SessionID:         req.ThreadID,
		RequestedTargetID: req.WorkspaceTargetID,
		Principal:         principal,
		BestEffort:        bestEffort,
	})
	if err != nil {
		return ctx, req, err
	}
	req.WorkspaceTargetID = ws.TargetID
	req.WorkspaceTarget = ws.Target
	return ctx, req, nil
}

// userPrincipal is the principal of an entry whose run is started by an end
// user.
func userPrincipal(req ChatRequest) *workspacePrincipal {
	return &workspacePrincipal{UserID: req.UserID}
}

func (s *Service) resolveWorkspaceTargetSnapshot(ctx context.Context, botID, targetID string) (*WorkspaceTarget, error) {
	if s == nil || s.workspaceTargets == nil {
		if strings.TrimSpace(targetID) == "" {
			return nil, nil
		}
		return nil, errors.New("workspace target resolver not configured")
	}
	resolved, err := s.workspaceTargets.ResolveWorkspaceTarget(ctx, botID, targetID)
	if err != nil {
		return nil, err
	}
	return &WorkspaceTarget{
		TargetID: strings.TrimSpace(resolved.TargetID),
		Kind:     strings.TrimSpace(resolved.Kind),
		Name:     strings.TrimSpace(resolved.Name),
	}, nil
}

func workspaceTargetFromRunConfig(cfg native.RunConfig) *WorkspaceTarget {
	if strings.TrimSpace(cfg.Identity.WorkspaceTargetID) == "" {
		return nil
	}
	return &WorkspaceTarget{
		TargetID: strings.TrimSpace(cfg.Identity.WorkspaceTargetID),
		Kind:     strings.TrimSpace(cfg.Identity.WorkspaceTargetKind),
		Name:     strings.TrimSpace(cfg.Identity.WorkspaceTargetName),
	}
}

func rejectExternalAgentWorkspaceTarget(req ChatRequest) error {
	if strings.TrimSpace(req.WorkspaceTargetID) == "" {
		return nil
	}
	return ErrExternalAgentWorkspaceTargetUnsupported
}
