package decide

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/sessionfacts"
	"github.com/felinics/memoh/internal/workspace"
)

type denyToolApprovalPolicyProvider struct{}

func (denyToolApprovalPolicyProvider) ToolApprovalPolicy(context.Context, string) (toolapproval.PolicyConfig, error) {
	return toolapproval.PolicyConfig{
		Enabled: true,
		Read:    toolapproval.FilePolicy{Mode: toolapproval.PolicyModeDeny},
	}, nil
}

type workspaceTargetPolicyErrorResolver struct {
	err error
}

func (r workspaceTargetPolicyErrorResolver) ResolveWorkspaceTargetPolicy(context.Context, string, string) (toolapproval.WorkspaceTargetPolicy, error) {
	return toolapproval.WorkspaceTargetPolicy{}, r.err
}

func TestToolApprovalPolicyDenyWinsOverHookForcedApproval(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	approvalService := toolapproval.NewService(log, nil, denyToolApprovalPolicyProvider{})
	resolver := newTestDecider(Deps{ToolApproval: approvalService})
	handler := resolver.ApprovalHandler(HandlerParams{
		BotID:        "11111111-1111-1111-1111-111111111111",
		SessionID:    "22222222-2222-2222-2222-222222222222",
		DecisionMode: sessionfacts.Preset(sessionmode.Chat).DecisionMode,
	})

	result, err := handler(native.ContextWithHookForcedApproval(context.Background(), "hook asks for review"), sdk.ToolCall{
		ToolCallID: "call-1",
		ToolName:   "read",
		Input:      toolexec.ArgumentsFromValue(map[string]any{"path": "/data/file.txt"}),
	})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if result.Decision != toolexec.ToolApprovalDecisionRejected || result.Reason != toolapproval.PolicyDeniedReason {
		t.Fatalf("result = %+v, want policy rejection", result)
	}
}

func TestToolApprovalHandlerOnlyRecoversMissingWorkspaceTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		resolveErr   error
		wantApproved bool
	}{
		{name: "missing target", resolveErr: workspace.ErrWorkspaceTargetNotFound, wantApproved: true},
		{name: "offline target", resolveErr: workspace.ErrRemoteRuntimeOffline},
		{name: "internal failure", resolveErr: errors.New("database unavailable")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			log := slog.New(slog.DiscardHandler)
			approvalService := toolapproval.NewService(log, nil, nil)
			approvalService.SetWorkspaceTargetPolicyResolver(workspaceTargetPolicyErrorResolver{err: tt.resolveErr})
			resolver := newTestDecider(Deps{ToolApproval: approvalService, Logger: log})
			handler := resolver.ApprovalHandler(HandlerParams{
				BotID:        "bot-1",
				SessionID:    "session-1",
				DecisionMode: sessionfacts.Preset(sessionmode.Chat).DecisionMode,
			})

			result, err := handler(context.Background(), sdk.ToolCall{
				ToolCallID: "call-1",
				ToolName:   "exec",
				Input: toolexec.ArgumentsFromValue(map[string]any{
					"command":   "node --version",
					"target_id": "server_workspace",
				}),
			})
			if tt.wantApproved {
				if err != nil {
					t.Fatalf("handler returned error: %v", err)
				}
				if result.Decision != toolexec.ToolApprovalDecisionApproved {
					t.Fatalf("decision = %q, want approved", result.Decision)
				}
				return
			}
			if !errors.Is(err, tt.resolveErr) {
				t.Fatalf("handler error = %v, want %v", err, tt.resolveErr)
			}
		})
	}
}

func TestApprovalResultMetadata(t *testing.T) {
	t.Parallel()

	got := ApprovalResultMetadata(toolapproval.Request{
		ShortID:    7,
		Status:     toolapproval.StatusRejected,
		ToolName:   "exec",
		ToolCallID: "call-1",
	})

	if got["short_id"] != 7 ||
		got["status"] != toolapproval.StatusRejected ||
		got["tool_name"] != "exec" ||
		got["tool_call_id"] != "call-1" {
		t.Fatalf("unexpected metadata: %#v", got)
	}
}

func TestServiceLimitToolResultTextUsesAgentLimits(t *testing.T) {
	t.Parallel()

	r := newTestDecider(Deps{
		Limits: native.New(native.Deps{
			Limits: native.Limits{ToolOutputMaxBytes: 512, ToolOutputMaxLines: 80},
		}),
	})
	large := "HEAD\n" + strings.Repeat("rejected detail ", 300) + "\nTAIL"

	got := r.LimitToolResultText(large, "write")
	if len(got) >= len(large) {
		t.Fatalf("tool result text was not pruned: got %d bytes, original %d", len(got), len(large))
	}
	if !strings.Contains(got, "[memoh pruned]") {
		t.Fatalf("tool result text missing prune marker:\n%s", got)
	}
}

type workspaceTargetPolicyStubResolver struct {
	policy toolapproval.WorkspaceTargetPolicy
}

func (r workspaceTargetPolicyStubResolver) ResolveWorkspaceTargetPolicy(context.Context, string, string) (toolapproval.WorkspaceTargetPolicy, error) {
	return r.policy, nil
}

// The policy resolves the workspace target on a copy of the call arguments.
// A bypass must hand that copy back so the tool executes on the target the
// policy evaluated and the persisted call names it.
func TestToolApprovalHandlerPinsWorkspaceTargetOnBypass(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	approvalService := toolapproval.NewService(log, nil, nil)
	approvalService.SetWorkspaceTargetPolicyResolver(workspaceTargetPolicyStubResolver{policy: toolapproval.WorkspaceTargetPolicy{
		TargetID: "canonical-target",
		Kind:     "remote",
		Name:     "Office Mac",
		Config:   toolapproval.PolicyConfig{Enabled: true, Read: toolapproval.FilePolicy{Mode: toolapproval.PolicyModeAllow}},
	}})
	resolver := newTestDecider(Deps{ToolApproval: approvalService, Logger: log})
	handler := resolver.ApprovalHandler(HandlerParams{
		BotID:        "bot-1",
		SessionID:    "session-1",
		DecisionMode: sessionfacts.Preset(sessionmode.Chat).DecisionMode,
	})

	result, err := handler(context.Background(), sdk.ToolCall{
		ToolCallID: "call-1",
		ToolName:   "read",
		Input:      toolexec.ArgumentsFromValue(map[string]any{"path": "/data/file.txt", "target_id": "requested-target"}),
	})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if result.Decision != toolexec.ToolApprovalDecisionApproved {
		t.Fatalf("decision = %q, want approved", result.Decision)
	}
	if result.Input == nil {
		t.Fatal("approval did not hand back the evaluated arguments")
	}
	args, _ := toolexec.ArgumentsValue(*result.Input).(map[string]any)
	if args["target_id"] != "canonical-target" || args["path"] != "/data/file.txt" {
		t.Fatalf("approved arguments = %#v, want the canonical target pinned", args)
	}
}
