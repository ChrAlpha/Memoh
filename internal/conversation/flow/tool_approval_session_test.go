package flow

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/agent/sessionmode"
	"github.com/memohai/memoh/internal/models"
	"github.com/memohai/memoh/internal/session"
	"github.com/memohai/memoh/internal/settings"
	"github.com/memohai/memoh/internal/toolapproval"
)

func TestIsInteractiveApprovalSession(t *testing.T) {
	t.Parallel()

	for _, sessionType := range []string{"", sessionmode.Chat, "CHAT", sessionmode.ACPAgent} {
		if !isInteractiveApprovalSession(sessionType) {
			t.Fatalf("expected %q to allow interactive approvals", sessionType)
		}
	}

	for _, sessionType := range []string{sessionmode.Discuss, sessionmode.Schedule, sessionmode.Heartbeat, sessionmode.Subagent} {
		if isInteractiveApprovalSession(sessionType) {
			t.Fatalf("expected %q to reject interactive approvals", sessionType)
		}
	}
}

func TestToolApprovalHandlerLimitsForcedApprovalRejectionReason(t *testing.T) {
	t.Parallel()

	large := "HEAD\n" + strings.Repeat("rejected detail ", 300) + "\nTAIL"
	resolver := &Resolver{
		agent: agentpkg.New(agentpkg.Deps{
			Limits: agentpkg.Limits{ToolOutputMaxBytes: 512, ToolOutputMaxLines: 80},
		}),
	}
	handler := resolver.buildToolApprovalHandler(baseRunConfigParams{
		BotID:       "bot-1",
		SessionID:   "session-1",
		SessionType: sessionmode.Chat,
	})

	result, err := handler(agentpkg.ContextWithHookForcedApproval(context.Background(), large), sdk.ToolCall{
		ToolCallID: "call-1",
		ToolName:   "write",
		Input:      map[string]any{},
	})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if result.Decision != sdk.ToolApprovalDecisionRejected {
		t.Fatalf("decision = %q, want rejected", result.Decision)
	}
	if len(result.Reason) >= len(large) {
		t.Fatalf("approval reason was not pruned: got %d bytes, original %d", len(result.Reason), len(large))
	}
	if !strings.Contains(result.Reason, "[memoh pruned]") {
		t.Fatalf("approval reason missing prune marker:\n%s", result.Reason)
	}
}

func TestAgentSessionModesMatchPersistedSessionTypes(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		sessionmode.Chat:      session.TypeChat,
		sessionmode.Heartbeat: session.TypeHeartbeat,
		sessionmode.Schedule:  session.TypeSchedule,
		sessionmode.Subagent:  session.TypeSubagent,
		sessionmode.Discuss:   session.TypeDiscuss,
		sessionmode.ACPAgent:  session.TypeACPAgent,
	}
	for got, want := range cases {
		if got != want {
			t.Fatalf("agent session mode %q must match persisted type %q", got, want)
		}
	}
}

func TestResolveRunConfigSessionTypeUsesStoredSessionType(t *testing.T) {
	t.Parallel()

	resolver := &Resolver{
		sessionService: &fakeBackgroundSessionService{
			getFn: func(_ context.Context, sessionID string) (session.Session, error) {
				if sessionID != "session-1" {
					t.Fatalf("unexpected session id: %s", sessionID)
				}
				return session.Session{ID: sessionID, Type: session.TypeChat}, nil
			},
		},
	}

	if got := resolver.resolveRunConfigSessionType(context.Background(), "session-1"); got != session.TypeChat {
		t.Fatalf("session type = %q, want %q", got, session.TypeChat)
	}
}

func TestResolveRunConfigSessionTypeFallsBackToChat(t *testing.T) {
	t.Parallel()

	resolver := &Resolver{
		sessionService: &fakeBackgroundSessionService{
			getFn: func(context.Context, string) (session.Session, error) {
				return session.Session{}, errors.New("db unavailable")
			},
		},
	}

	if got := resolver.resolveRunConfigSessionType(context.Background(), "session-1"); got != session.TypeChat {
		t.Fatalf("session type = %q, want %q", got, session.TypeChat)
	}
}

func TestResolveRunConfigSkipsModelResolutionForACPRuntime(t *testing.T) {
	t.Parallel()

	resolver := &Resolver{
		sessionService: &fakeBackgroundSessionService{
			getFn: func(_ context.Context, sessionID string) (session.Session, error) {
				if sessionID != "session-1" {
					t.Fatalf("unexpected session id: %s", sessionID)
				}
				return session.Session{
					ID:          sessionID,
					Type:        session.TypeDiscuss,
					SessionMode: session.TypeDiscuss,
					RuntimeType: session.RuntimeACPAgent,
				}, nil
			},
		},
	}

	got, err := resolver.ResolveRunConfig(context.Background(), "bot-1", "session-1", "user-1", "telegram", "", "group", "")
	if err != nil {
		t.Fatalf("ResolveRunConfig() error = %v", err)
	}
	if got.RuntimeType != session.RuntimeACPAgent {
		t.Fatalf("runtime type = %q, want %q", got.RuntimeType, session.RuntimeACPAgent)
	}
	if got.RunConfig.SessionType != session.TypeDiscuss {
		t.Fatalf("run config session type = %q, want %q", got.RunConfig.SessionType, session.TypeDiscuss)
	}
	if got.ModelID != "" || got.RunConfig.Model != nil {
		t.Fatalf("ACP runtime should not resolve a model, model_id=%q model=%#v", got.ModelID, got.RunConfig.Model)
	}
	if got.ContextBudgetMaxTokens != 0 {
		t.Fatalf("ACP runtime should not resolve a context budget, got %d", got.ContextBudgetMaxTokens)
	}
}

func TestResolveRunConfigPopulatesContextBudgetMaxTokensFromChatModel(t *testing.T) {
	t.Parallel()

	conn, queries := newModelSelectionTestDB(t)
	const modelID = "00000000-0000-0000-0000-000000000601"
	const providerID = "00000000-0000-0000-0000-000000000602"
	insertModelSelectionProvider(t, conn, providerID, "openai-completions", true)
	insertModelSelectionModel(t, conn, modelID, "gpt-run-config-context-window", providerID, models.ModelTypeChat, true, `{"context_window": 128000}`)

	resolver := &Resolver{
		modelsService:   models.NewService(slog.New(slog.DiscardHandler), queries),
		queries:         queries,
		settingsService: settings.NewService(slog.New(slog.DiscardHandler), &acpContextBudgetSettingsQueries{chatModelID: modelID}, nil, nil),
		sessionService: &fakeBackgroundSessionService{
			getFn: func(_ context.Context, sessionID string) (session.Session, error) {
				return session.Session{
					ID:          sessionID,
					BotID:       storeRoundBotID,
					Type:        session.TypeChat,
					RuntimeType: session.RuntimeModel,
				}, nil
			},
		},
		logger: slog.New(slog.DiscardHandler),
	}

	got, err := resolver.ResolveRunConfig(context.Background(), storeRoundBotID, "session-1", "user-1", "web", "", "", "")
	if err != nil {
		t.Fatalf("ResolveRunConfig() error = %v", err)
	}
	if got.ContextBudgetMaxTokens != 128000 {
		t.Fatalf("ContextBudgetMaxTokens = %d, want 128000", got.ContextBudgetMaxTokens)
	}
}

func TestApprovalResultMetadata(t *testing.T) {
	t.Parallel()

	got := approvalResultMetadata(toolapproval.Request{
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

func TestResolverLimitToolResultTextUsesAgentLimits(t *testing.T) {
	t.Parallel()

	r := &Resolver{
		agent: agentpkg.New(agentpkg.Deps{
			Limits: agentpkg.Limits{ToolOutputMaxBytes: 512, ToolOutputMaxLines: 80},
		}),
	}
	large := "HEAD\n" + strings.Repeat("rejected detail ", 300) + "\nTAIL"

	got := r.limitToolResultText(large, "write")
	if len(got) >= len(large) {
		t.Fatalf("tool result text was not pruned: got %d bytes, original %d", len(got), len(large))
	}
	if !strings.Contains(got, "[memoh pruned]") {
		t.Fatalf("tool result text missing prune marker:\n%s", got)
	}
}
