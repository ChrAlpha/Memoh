package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/lifecycle"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/partmeta"
	acpagent "github.com/felinics/memoh/internal/agent/runtime/acp"
	acpclient "github.com/felinics/memoh/internal/agent/runtime/acp/client"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/apperror"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	memprovider "github.com/felinics/memoh/internal/memory/adapters"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/settings"
)

func TestStreamACPAgentWSPromptBytesMatchQuery(t *testing.T) {
	t.Parallel()

	pool := &recordingACPPrompter{result: acpclient.PromptResult{Text: "ok", StopReason: "end_turn"}}
	resolver := newTestService(t, Deps{
		Messages:       &agenttest.RecordingMessages{},
		BotPermissions: allowWorkspaceExecFor("user-1"),
		Sessions: &agenttest.Sessions{
			GetFn: func(_ context.Context, _ string) (session.Thread, error) {
				return session.Thread{
					ID:    "session-1",
					BotID: "bot-1",
					Type:  session.TypeACPAgent,
					Metadata: map[string]any{
						"acp_agent_id":             "codex",
						"project_path":             "/data/app",
						"runtime_owner_account_id": "user-1",
					},
				}, nil
			},
			UpdateMetadataFn: func(_ context.Context, sessionID string, _ map[string]any) (session.Thread, error) {
				return session.Thread{ID: sessionID}, nil
			},
		},
	}, withWebAdmitter(), withACPPool(pool))

	if _, err := streamWebSend(context.Background(), t, resolver, runreq.ChatRequest{
		BotID:    "bot-1",
		ThreadID: "session-1",
		Query:    "  inspect the app  ",
	}); err != nil {
		t.Fatalf("Web send failed: %v", err)
	}
	if pool.input.Prompt != "inspect the app" {
		t.Fatalf("Prompt = %q, want trimmed query bytes", pool.input.Prompt)
	}
	if strings.Contains(pool.input.ContextMarkdown, "inspect the app") {
		t.Fatalf("query must not join the context document: %q", pool.input.ContextMarkdown)
	}
}

func TestStreamACPAgentWSPassesTheAdmittedRunAndRecordsACompletedLifecycle(t *testing.T) {
	t.Parallel()

	pool := &recordingACPPrompter{result: acpclient.PromptResult{Text: "done", StopReason: "end_turn"}}
	messages := &agenttest.RecordingMessages{}
	lifecycles := &agenttest.RecordingLifecycleStore{}
	service := newACPLifecycleService(t, pool, messages, lifecycles)

	_, err := streamExternalWebSend(context.Background(), service, runreq.ChatRequest{
		BotID:    agenttest.LifecycleBotID,
		ThreadID: agenttest.LifecycleSessionID,
		RunID:    agenttest.LifecycleRunID,
		Query:    "PRIVATE_ACP_PROMPT",
	}, make(chan runreq.WSStreamEvent, 8), make(chan struct{}))
	if err != nil {
		t.Fatalf("streamACPAgentWS() error = %v", err)
	}
	if pool.input.RunID != agenttest.LifecycleRunID {
		t.Fatalf("ACP prompt RunID = %q, want admitted RunID %q", pool.input.RunID, agenttest.LifecycleRunID)
	}
	row, snapshot := requireACPLifecycle(t, lifecycles, agenttest.LifecycleRunID, lifecycle.StatusCompleted)
	if row.ErrorCode.Valid {
		t.Fatalf("completed lifecycle error code = %#v, want none", row.ErrorCode)
	}
	if snapshot.Version != contextfrag.LifecycleSnapshotVersion || snapshot.View != contextfrag.ViewExternalAgentPrompt || snapshot.Counts.Fragments == 0 {
		t.Fatalf("ACP context-view snapshot = %#v, want populated ACP manifest", snapshot)
	}
	if len(snapshot.SelectionDecisions) == 0 {
		t.Fatalf("ACP context-view snapshot omitted selection decisions: %#v", snapshot)
	}
	if snapshot.AssistantMessageID != "message-id" {
		t.Fatalf("assistant message ID = %q, want message-id", snapshot.AssistantMessageID)
	}
	if strings.Contains(string(row.Snapshot), "PRIVATE_ACP_PROMPT") {
		t.Fatalf("content-light lifecycle leaked prompt: %s", row.Snapshot)
	}
	last := messages.Persisted[len(messages.Persisted)-1]
	if _, ok := last.Metadata[contextfrag.MetadataContextLifecycleKey].(contextfrag.LifecycleSnapshot); !ok {
		t.Fatalf("assistant lifecycle metadata = %#v, want snapshot", last.Metadata)
	}
}

func TestStreamACPAgentWSProviderFailurePersistsFailedLifecycle(t *testing.T) {
	t.Parallel()

	pool := &recordingACPPrompter{err: errors.New("PRIVATE_ACP_PROVIDER_FAILURE")}
	lifecycles := &agenttest.RecordingLifecycleStore{}
	service := newACPLifecycleService(t, pool, &agenttest.RecordingMessages{}, lifecycles)

	if _, err := streamExternalWebSend(context.Background(), service, runreq.ChatRequest{
		BotID:    agenttest.LifecycleBotID,
		ThreadID: agenttest.LifecycleSessionID,
		RunID:    agenttest.LifecycleRunID,
		Query:    "inspect",
	}, make(chan runreq.WSStreamEvent, 8), make(chan struct{})); err != nil {
		t.Fatalf("streamACPAgentWS() error = %v", err)
	}
	row, _ := requireACPLifecycle(t, lifecycles, agenttest.LifecycleRunID, lifecycle.StatusFailedProvider)
	if row.ErrorCode.Valid {
		t.Fatalf("private provider failure became stable error code: %#v", row.ErrorCode)
	}
	if strings.Contains(string(row.Snapshot), "PRIVATE_ACP_PROVIDER_FAILURE") {
		t.Fatalf("content-light lifecycle leaked provider diagnostic: %s", row.Snapshot)
	}
}

func TestStreamACPAgentWSExplicitAbortPersistsAbortedLifecycle(t *testing.T) {
	started := make(chan struct{})
	pool := &recordingACPPrompter{
		promptFn: func(ctx context.Context, _ acpagent.PromptInput) (acpclient.PromptResult, error) {
			close(started)
			<-ctx.Done()
			return acpclient.PromptResult{}, context.Cause(ctx)
		},
	}
	lifecycles := &agenttest.RecordingLifecycleStore{}
	service := newACPLifecycleService(t, pool, &agenttest.RecordingMessages{}, lifecycles)
	abortCh := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ignoreOutcome(streamExternalWebSend(context.Background(), service, runreq.ChatRequest{
			BotID:    agenttest.LifecycleBotID,
			ThreadID: agenttest.LifecycleSessionID,
			RunID:    agenttest.LifecycleRunID,
			Query:    "inspect",
		}, make(chan runreq.WSStreamEvent, 8), abortCh))
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("ACP prompt did not start")
	}
	close(abortCh)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("streamACPAgentWS() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("aborted ACP prompt did not return")
	}
	requireACPLifecycle(t, lifecycles, agenttest.LifecycleRunID, lifecycle.StatusAborted)
}

func TestStreamACPAgentWSTransformedConfigFailurePersistsStableCode(t *testing.T) {
	t.Parallel()

	pool := &recordingACPPrompter{err: fmt.Errorf("%w: PRIVATE_CONFIG_DETAIL", acpclient.ErrModelUnavailable)}
	lifecycles := &agenttest.RecordingLifecycleStore{}
	service := newACPLifecycleService(t, pool, &agenttest.RecordingMessages{}, lifecycles)

	runResult, err := streamExternalWebSend(context.Background(), service, runreq.ChatRequest{
		BotID:    agenttest.LifecycleBotID,
		ThreadID: agenttest.LifecycleSessionID,
		RunID:    agenttest.LifecycleRunID,
		Query:    "inspect",
	}, make(chan runreq.WSStreamEvent, 8), make(chan struct{}))
	if err != nil || runResult.ErrorCode() != string(apperror.CodeACPModelUnavailable) {
		t.Fatalf("streamACPAgentWS() = %q, %v, want %q", runResult.ErrorCode(), err, apperror.CodeACPModelUnavailable)
	}
	row, _ := requireACPLifecycle(t, lifecycles, agenttest.LifecycleRunID, lifecycle.StatusFailedProvider)
	if !row.ErrorCode.Valid || row.ErrorCode.String != string(apperror.CodeACPModelUnavailable) {
		t.Fatalf("lifecycle error code = %#v, want %q", row.ErrorCode, apperror.CodeACPModelUnavailable)
	}
	if strings.Contains(string(row.Snapshot), "PRIVATE_CONFIG_DETAIL") {
		t.Fatalf("content-light lifecycle leaked config diagnostic: %s", row.Snapshot)
	}
}

func TestWebSendRoutesACPRuntimeSessionToACPPool(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	pool := &recordingACPPrompter{
		result: withTranscriptOutput(acpclient.PromptResult{
			Text:       "done from codex",
			StopReason: "end_turn",
			Usage:      &sdk.Usage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8},
		}),
	}
	resolver := newTestService(t, Deps{
		Messages:       messages,
		BotPermissions: allowWorkspaceExecFor("user-1"),
		Sessions: &agenttest.Sessions{
			GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
				if sessionID != "session-1" {
					t.Fatalf("unexpected session id: %s", sessionID)
				}
				return session.Thread{
					ID:    "session-1",
					BotID: "bot-1",
					Type:  session.TypeACPAgent,
					Metadata: map[string]any{
						"acp_agent_id":             "codex",
						"project_path":             "/data/app",
						"runtime_owner_account_id": "user-1",
					},
				}, nil
			},
			UpdateMetadataFn: func(_ context.Context, sessionID string, _ map[string]any) (session.Thread, error) {
				return session.Thread{ID: sessionID}, nil
			},
		},
	}, withWebAdmitter(), withACPPool(pool))

	eventCh, err := streamWebSend(context.Background(), t, resolver, runreq.ChatRequest{
		BotID:           "bot-1",
		ThreadID:        "session-1",
		Query:           "inspect the app",
		Model:           "gpt-5.1-codex",
		ReasoningEffort: "high",
		Attachments: []runreq.ChatAttachment{{
			Type:   "image",
			Base64: agenttest.DataURL("image/png", agenttest.RasterPNG(t)),
			Mime:   "image/png",
			Name:   "screenshot.png",
		}},
		ReplyAttachments: []runreq.ChatAttachment{{
			Type: "file",
			Name: "previous.log",
			URL:  "https://example.com/previous.log",
		}},
	})
	if err != nil {
		t.Fatalf("Web send failed: %v", err)
	}

	if pool.calls != 1 {
		t.Fatalf("ACP pool calls = %d, want 1", pool.calls)
	}
	if pool.input.BotID != "bot-1" || pool.input.SessionID != "session-1" || pool.input.AgentID != "codex" || pool.input.ProjectPath != "/data/app" {
		t.Fatalf("ACP prompt input = %#v", pool.input)
	}
	if pool.input.ModelID != "gpt-5.1-codex" || pool.input.ReasoningEffort != "high" {
		t.Fatalf("ACP turn config = model %q reasoning %q", pool.input.ModelID, pool.input.ReasoningEffort)
	}
	if pool.input.ContextURI != "memoh://context/current-turn" || !strings.Contains(pool.input.ContextMarkdown, "## Current Runtime") || !strings.Contains(pool.input.ContextMarkdown, "Bot ID: bot-1") {
		t.Fatalf("Runtime context = uri %q markdown %q, want dynamic Memoh context", pool.input.ContextURI, pool.input.ContextMarkdown)
	}
	if pool.input.ContextBudgetMaxTokens != 0 {
		t.Fatalf("ContextBudgetMaxTokens = %d, want 0 when models/settings services are not configured", pool.input.ContextBudgetMaxTokens)
	}
	if len(pool.input.Images) != 1 ||
		pool.input.Images[0].Data != base64.StdEncoding.EncodeToString(agenttest.RasterPNG(t)) ||
		pool.input.Images[0].MimeType != "image/png" {
		t.Fatalf("ACP prompt images = %#v, want inline PNG", pool.input.Images)
	}
	if len(pool.input.AttachmentReferences) != 1 || pool.input.AttachmentReferences[0] != "https://example.com/previous.log" {
		t.Fatalf("ACP attachment references = %#v, want reply attachment URL", pool.input.AttachmentReferences)
	}
	if !strings.Contains(pool.input.ContextMarkdown, "previous.log") || !strings.Contains(pool.input.ContextMarkdown, "https://example.com/previous.log") {
		t.Fatalf("Runtime context = %q, want reply attachment", pool.input.ContextMarkdown)
	}
	if len(messages.Persisted) != 2 {
		t.Fatalf("persisted %d messages, want user + assistant", len(messages.Persisted))
	}
	if messages.Persisted[0].Role != "user" || messages.Persisted[1].Role != "assistant" {
		t.Fatalf("persisted roles = %q, %q", messages.Persisted[0].Role, messages.Persisted[1].Role)
	}
	if got := messages.Persisted[1].Metadata["acp_agent_id"]; got != "codex" {
		t.Fatalf("assistant acp_agent_id = %#v, want codex", got)
	}
	if got := persistedText(t, messages.Persisted[1].Content); got != "done from codex" {
		t.Fatalf("persisted assistant text = %q, want done from codex", got)
	}

	events := drainAgentEvents(t, eventCh)
	if !containsStreamEvent(events, native.EventStart) || !containsStreamEvent(events, native.EventEnd) {
		t.Fatalf("events = %#v, want agent start/end", events)
	}
	if !containsTextDelta(events, "streamed from acp") {
		t.Fatalf("events = %#v, want ACP stream delta", events)
	}
	end := requireStreamEvent(t, events, native.EventEnd)
	if got := terminalAssistantText(t, end); got != "done from codex" {
		t.Fatalf("terminal assistant text = %q, want done from codex", got)
	}
	var usage sdk.Usage
	if err := json.Unmarshal(end.Usage, &usage); err != nil {
		t.Fatalf("decode terminal usage: %v", err)
	}
	if usage.InputTokens != 3 || usage.OutputTokens != 5 || usage.TotalTokens != 8 {
		t.Fatalf("terminal usage = %+v, want input=3 output=5 total=8", usage)
	}
}

func TestWebSendRoutesDiscussACPAgentSessionToACPPool(t *testing.T) {
	t.Parallel()

	pool := &recordingACPPrompter{
		result: withTranscriptOutput(acpclient.PromptResult{
			Text:       "done from codex",
			StopReason: "end_turn",
		}),
	}
	resolver := newTestService(t, Deps{
		Messages:       &agenttest.RecordingMessages{},
		BotPermissions: allowWorkspaceExecFor("user-1"),
		Sessions: &agenttest.Sessions{
			GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
				if sessionID != "session-1" {
					t.Fatalf("unexpected session id: %s", sessionID)
				}
				return session.Thread{
					ID:          "session-1",
					BotID:       "bot-1",
					Type:        session.TypeDiscuss,
					SessionMode: session.TypeDiscuss,
					RuntimeType: session.RuntimeACPAgent,
					RuntimeMetadata: map[string]any{
						"acp_agent_id":             "codex",
						"project_path":             "/data/app",
						"runtime_owner_account_id": "user-1",
					},
				}, nil
			},
			UpdateMetadataFn: func(_ context.Context, sessionID string, _ map[string]any) (session.Thread, error) {
				return session.Thread{ID: sessionID}, nil
			},
		},
	}, withWebAdmitter(), withACPPool(pool))

	eventCh, err := streamWebSend(context.Background(), t, resolver, runreq.ChatRequest{
		BotID:    "bot-1",
		ThreadID: "session-1",
		Query:    "inspect the group thread",
	})
	if err != nil {
		t.Fatalf("Web send failed: %v", err)
	}
	events := drainAgentEvents(t, eventCh)
	if pool.calls != 1 {
		t.Fatalf("ACP pool calls = %d, want 1", pool.calls)
	}
	if pool.input.BotID != "bot-1" || pool.input.SessionID != "session-1" || pool.input.AgentID != "codex" || pool.input.ProjectPath != "/data/app" {
		t.Fatalf("ACP prompt input = %#v", pool.input)
	}
	if !containsStreamEvent(events, native.EventStart) || !containsStreamEvent(events, native.EventEnd) {
		t.Fatalf("events = %#v, want agent start/end", events)
	}
	if !containsTextDelta(events, "streamed from acp") {
		t.Fatalf("events = %#v, want ACP stream delta", events)
	}
}

func TestWebSendRejectsACPBotMismatchBeforePersistence(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	pool := &recordingACPPrompter{}
	resolver := newTestService(t, Deps{
		Messages:       messages,
		BotPermissions: allowWorkspaceExecFor("user-1"),
		Sessions: &agenttest.Sessions{
			GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
				if sessionID != "session-1" {
					t.Fatalf("unexpected session id: %s", sessionID)
				}
				return session.Thread{
					ID:    "session-1",
					BotID: "bot-2",
					Type:  session.TypeACPAgent,
					Metadata: map[string]any{
						"acp_agent_id":             "codex",
						"project_path":             "/data/app",
						"runtime_owner_account_id": "user-1",
					},
				}, nil
			},
			UpdateMetadataFn: func(_ context.Context, sessionID string, _ map[string]any) (session.Thread, error) {
				return session.Thread{ID: sessionID}, nil
			},
		},
	}, withWebAdmitter(), withACPPool(pool))

	_, err := streamWebSend(context.Background(), t, resolver, runreq.ChatRequest{
		BotID:    "bot-1",
		ThreadID: "session-1",
		Query:    "inspect the app",
	})
	if err == nil {
		t.Fatal("Web send succeeded, want bot mismatch")
	}
	if pool.calls != 0 {
		t.Fatalf("ACP pool calls = %d, want 0", pool.calls)
	}
	if len(messages.Persisted) != 0 {
		t.Fatalf("persisted %d messages, want 0", len(messages.Persisted))
	}
}

// Single-session execution is no longer this layer's guarantee, and there is no
// test for it here on purpose. An in-process check could only answer for one
// server, so it moved to durable admission, where a concurrent invocation is
// refused with a retryable session_busy before any runtime is asked to prompt.
// See TestAdmitAnswersBusyForConcurrentInvocation in the sessionruntime package,
// which also covers what the in-process version could not express: the rejected
// invocation is admitted normally once the active run reaches a terminal state.

func TestStreamACPAgentWSRechecksRuntimeOwnerWorkspaceExecBeforePrompt(t *testing.T) {
	t.Parallel()

	pool := &recordingACPPrompter{
		result: acpclient.PromptResult{
			Text:       "should not run",
			StopReason: "end_turn",
		},
	}
	resolver := newTestService(t, Deps{
		Messages:       &agenttest.RecordingMessages{},
		BotPermissions: &agenttest.PermissionChecker{Values: map[string]bool{}},
		Sessions:       acpRuntimeSessionServiceForTest("user-1"),
	}, withWebAdmitter(), withACPPool(pool))

	runResult, err := streamExternalWebSend(
		context.Background(), resolver,
		runreq.ChatRequest{
			BotID:    "bot-1",
			ThreadID: "session-1",
			Query:    "inspect the app",
		},
		make(chan runreq.WSStreamEvent, 8),
		make(chan struct{}),
	)
	if err != nil || runResult.ErrorCode() != string(apperror.CodeNoWorkspaceExec) {
		t.Fatalf("streamACPAgentWS() = %q, %v, want %s", runResult.ErrorCode(), err, apperror.CodeNoWorkspaceExec)
	}
	if pool.calls != 0 {
		t.Fatalf("ACP pool calls = %d, want 0 when runtime owner lost workspace_exec", pool.calls)
	}
}

func TestWebSendPersistsACPUserInputProjectionOnceBeforePromptReturns(t *testing.T) {
	t.Parallel()
	turnPosition := int64(7)

	streamed := []event.StreamEvent{
		{
			Type:       event.ToolCallStart,
			ToolCallID: "ask-1",
			ToolName:   userinput.ToolNameAskUser,
			Input: map[string]any{
				"questions": []any{
					map[string]any{
						"id":   "q1",
						"text": "Pick one",
						"type": "single_choice",
						"options": []any{
							map[string]any{"id": "a", "label": "A"},
						},
					},
				},
			},
		},
		{
			Type:        event.UserInputRequest,
			ToolCallID:  "ask-1",
			ToolName:    userinput.ToolNameAskUser,
			UserInputID: "input-1",
			ShortID:     1,
			Status:      userinput.StatusPending,
			Input: map[string]any{
				"questions": []any{
					map[string]any{
						"id":   "q1",
						"text": "Pick one",
						"type": "single_choice",
						"options": []any{
							map[string]any{"id": "a", "label": "A"},
						},
					},
				},
			},
			Metadata: map[string]any{
				"user_input_id": "input-1",
				"short_id":      1,
				"status":        userinput.StatusPending,
				"ui_payload": map[string]any{
					"questions": []any{
						map[string]any{
							"id":   "q1",
							"text": "Pick one",
							"type": "single_choice",
							"options": []any{
								map[string]any{"id": "a", "label": "A"},
							},
						},
					},
				},
			},
		},
		{
			Type:        event.UserInputRequest,
			ToolCallID:  "ask-1",
			ToolName:    userinput.ToolNameAskUser,
			UserInputID: "input-1",
			ShortID:     1,
			Status:      userinput.StatusCanceled,
			Input: map[string]any{
				"questions": []any{
					map[string]any{
						"id":   "q1",
						"text": "Pick one",
						"type": "single_choice",
						"options": []any{
							map[string]any{"id": "a", "label": "A"},
						},
					},
				},
			},
			Metadata: map[string]any{
				"user_input_id": "input-1",
				"short_id":      1,
				"status":        userinput.StatusCanceled,
				"ui_payload": map[string]any{
					"questions": []any{
						map[string]any{
							"id":   "q1",
							"text": "Pick one",
							"type": "single_choice",
							"options": []any{
								map[string]any{"id": "a", "label": "A"},
							},
						},
					},
				},
			},
		},
		{Type: event.TextDelta, Delta: "done"},
	}
	messages := &agenttest.RecordingMessages{}
	pool := &recordingACPPrompter{
		result:       withTranscriptOutput(acpclient.PromptResult{}, streamed...),
		streamEvents: streamed,
		afterEvents: func() {
			if len(messages.Persisted) != 2 {
				t.Fatalf("persisted before ACP prompt returned = %d, want user + initial decision projection", len(messages.Persisted))
			}
			if messages.Persisted[0].Role != "user" || messages.Persisted[1].Role != "assistant" {
				t.Fatalf("leading persisted roles = %q, %q", messages.Persisted[0].Role, messages.Persisted[1].Role)
			}
			if got := messages.Persisted[0]; got.RunID != "run-1" || got.TurnID != "turn-1" || got.TurnPosition == nil || *got.TurnPosition != turnPosition {
				t.Fatalf("leading user run/turn identity = (%q, %q, %v), want (run-1, turn-1, %d)", got.RunID, got.TurnID, got.TurnPosition, turnPosition)
			}
			if got := messages.Persisted[1]; got.RunID != "run-1" || got.TurnRequestMessageID != "message-id" {
				t.Fatalf("decision projection run/request identity = (%q, %q), want (run-1, message-id)", got.RunID, got.TurnRequestMessageID)
			}
		},
	}
	resolver := newTestService(t, Deps{
		Messages:       messages,
		BotPermissions: allowWorkspaceExecFor("user-1"),
		Sessions: &agenttest.Sessions{
			GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
				return session.Thread{
					ID:    sessionID,
					BotID: "bot-1",
					Type:  session.TypeACPAgent,
					Metadata: map[string]any{
						"acp_agent_id":             "codex",
						"project_path":             "/data/app",
						"runtime_owner_account_id": "user-1",
					},
				}, nil
			},
			UpdateMetadataFn: func(_ context.Context, sessionID string, _ map[string]any) (session.Thread, error) {
				return session.Thread{ID: sessionID}, nil
			},
		},
	}, withWebAdmission(runreq.ChatRequest{BotID: "bot-1", ThreadID: "session-1", RunID: "run-1", TurnID: "turn-1", TurnPosition: &turnPosition}), withACPPool(pool))

	if _, err := streamWebSend(context.Background(), t, resolver, runreq.ChatRequest{
		BotID:          "bot-1",
		ThreadID:       "session-1",
		RunID:          "run-1",
		TurnID:         "turn-1",
		TurnPosition:   &turnPosition,
		Query:          "inspect the app",
		CurrentChannel: "web",
	}); err != nil {
		t.Fatalf("Web send failed: %v", err)
	}

	if len(messages.Persisted) != 5 {
		t.Fatalf("persisted %d messages, want user + temporary projection + complete ACP transcript", len(messages.Persisted))
	}
	pendingProjection := persistedModelMessage(t, messages.Persisted[1].Content)
	pendingCalls := runreq.ExtractAssistantToolCallParts(pendingProjection)
	if len(pendingCalls) != 1 || pendingCalls[0].ToolCallID != "ask-1" {
		t.Fatalf("pending projected tool calls = %#v, want ask-1", pendingCalls)
	}
	if got := toolCallMetadataStatus(pendingCalls[0], "user_input"); got != userinput.StatusPending {
		t.Fatalf("pending projection status = %q, want pending", got)
	}
	terminalCall := persistedModelMessage(t, messages.Persisted[2].Content)
	terminalCalls := runreq.ExtractAssistantToolCallParts(terminalCall)
	if len(terminalCalls) != 1 || terminalCalls[0].ToolCallID != "ask-1" {
		t.Fatalf("terminal tool calls = %#v, want ask-1", terminalCalls)
	}
	if got := toolCallMetadataStatus(terminalCalls[0], "user_input"); got != userinput.StatusCanceled {
		t.Fatalf("terminal projection status = %q, want canceled", got)
	}
	results := runreq.ExtractToolResultParts(persistedModelMessage(t, messages.Persisted[3].Content))
	if len(results) != 1 || results[0].ToolCallID != "ask-1" || results[0].IsError {
		t.Fatalf("terminal user input result = %#v, want canceled ask-1 closure", results)
	}
	result, ok := toolexec.OutputValue(results[0].Result).(map[string]any)
	if !ok || result["status"] != userinput.StatusCanceled {
		t.Fatalf("terminal user input payload = %#v, want canceled", results[0].Result)
	}
	final := persistedModelMessage(t, messages.Persisted[4].Content)
	if got := final.TextContent(); got != "done" {
		t.Fatalf("final assistant text = %q, want done", got)
	}
	if len(messages.Deleted) != 1 || len(messages.Deleted[0]) != 1 {
		t.Fatalf("temporary projection cleanup = %#v, want one deleted message", messages.Deleted)
	}
}

func TestWebSendPersistsACPSubmittedUserInputResult(t *testing.T) {
	t.Parallel()

	questionInput := map[string]any{
		"questions": []any{
			map[string]any{
				"text": "Which dynasty?",
				"kind": "single_select",
				"options": []any{
					map[string]any{"label": "Xia"},
					map[string]any{"label": "Qin"},
				},
			},
		},
	}
	uiPayload := map[string]any{
		"version": 2,
		"questions": []any{
			map[string]any{
				"id":   "q1",
				"text": "Which dynasty?",
				"kind": "single_select",
				"options": []any{
					map[string]any{"id": "q1.o1", "label": "Xia"},
					map[string]any{"id": "q1.o2", "label": "Qin"},
				},
			},
		},
	}
	submitted := map[string]any{
		"status": userinput.StatusSubmitted,
		"answers": []any{
			map[string]any{
				"question_id": "q1",
				"question":    "Which dynasty?",
				"selected": []any{
					map[string]any{"id": "q1.o1", "label": "Xia"},
				},
			},
		},
	}
	userInputEvent := func(status string) event.StreamEvent {
		metadata := map[string]any{
			"user_input_id": "input-1",
			"short_id":      1,
			"status":        status,
			"ui_payload":    uiPayload,
		}
		if status == userinput.StatusSubmitted {
			metadata["answers"] = userinput.AnswersFromResult(submitted)
		}
		return event.StreamEvent{
			Type:        event.UserInputRequest,
			ToolCallID:  "ask-1",
			ToolName:    userinput.ToolNameAskUser,
			UserInputID: "input-1",
			ShortID:     1,
			Status:      status,
			Input:       questionInput,
			Metadata:    metadata,
		}
	}
	toolResult := map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": `{"status":"submitted"}`},
		},
		"structuredContent": submitted,
	}
	streamed := []event.StreamEvent{
		{Type: event.TextDelta, Delta: "Let me quiz you on Chinese history."},
		{
			Type:       event.ToolCallStart,
			ToolCallID: "ask-1",
			ToolName:   userinput.ToolNameAskUser,
			Input:      questionInput,
		},
		userInputEvent(userinput.StatusPending),
		userInputEvent(userinput.StatusSubmitted),
		{
			Type:       event.ToolCallEnd,
			ToolCallID: "ask-1",
			ToolName:   userinput.ToolNameAskUser,
			Result:     toolResult,
		},
		{Type: event.TextDelta, Delta: "Qin is correct."},
	}
	messages := &agenttest.RecordingMessages{}
	pool := &recordingACPPrompter{
		result:       withTranscriptOutput(acpclient.PromptResult{}, streamed...),
		streamEvents: streamed,
	}
	resolver := newTestService(t, Deps{
		Messages:       messages,
		BotPermissions: allowWorkspaceExecFor("user-1"),
		Sessions:       acpRuntimeSessionServiceForTest("user-1"),
	}, withWebAdmitter(), withACPPool(pool))

	if _, err := streamWebSend(context.Background(), t, resolver, runreq.ChatRequest{
		BotID:          "bot-1",
		ThreadID:       "session-1",
		Query:          "quiz me",
		CurrentChannel: "web",
	}); err != nil {
		t.Fatalf("Web send failed: %v", err)
	}

	if len(messages.Persisted) != 5 {
		t.Fatalf("persisted %d messages, want user + projection + narration + tool result + final assistant", len(messages.Persisted))
	}
	if got := persistedModelMessage(t, messages.Persisted[2].Content).TextContent(); got != "Let me quiz you on Chinese history." {
		t.Fatalf("persisted leading narration = %q", got)
	}
	ordered := runreq.ModelMessageToSDKMessage(persistedModelMessage(t, messages.Persisted[2].Content)).Content
	if len(ordered) != 2 {
		t.Fatalf("narration/tool content = %#v, want two ordered parts", ordered)
	}
	if text, ok := ordered[0].(sdk.TextPart); !ok || text.Text != "Let me quiz you on Chinese history." {
		t.Fatalf("first persisted part = %#v, want narration", ordered[0])
	}
	if call, ok := ordered[1].(sdk.ToolCallPart); !ok || call.ToolCallID != "ask-1" {
		t.Fatalf("second persisted part = %#v, want ask_user card", ordered[1])
	}
	if got := messages.Persisted[3].Role; got != "tool" {
		t.Fatalf("submitted result role = %q, want tool", got)
	}
	results := runreq.ExtractToolResultParts(persistedModelMessage(t, messages.Persisted[3].Content))
	if len(results) != 1 || results[0].ToolCallID != "ask-1" {
		t.Fatalf("persisted submitted results = %#v, want ask-1", results)
	}

	if len(messages.Deleted) != 1 || len(messages.Deleted[0]) != 1 {
		t.Fatalf("temporary projection cleanup = %#v, want one deleted message", messages.Deleted)
	}
	history := make([]runreq.ModelMessage, 0, len(messages.Persisted)-1)
	for index, persisted := range messages.Persisted {
		if index == 1 { // The temporary waiting projection was deleted.
			continue
		}
		history = append(history, persistedModelMessage(t, persisted.Content))
	}
	repaired := runreq.RepairToolCallClosures(runreq.NonNilModelMessages(runreq.SanitizeMessages(history)), runreq.SyntheticToolClosureError)
	projectionIndex := -1
	var repairedCalls, repairedResults int
	for index, message := range repaired {
		for _, call := range runreq.ExtractAssistantToolCallParts(message) {
			if call.ToolCallID == "ask-1" {
				repairedCalls++
				projectionIndex = index
			}
		}
		for _, result := range runreq.ExtractToolResultParts(message) {
			if result.ToolCallID != "ask-1" {
				continue
			}
			repairedResults++
			if result.IsError {
				t.Fatalf("next-turn history synthesized an error for submitted ask_user: %#v", result)
			}
		}
	}
	if repairedCalls != 1 || repairedResults != 1 {
		t.Fatalf("next-turn ask_user call/result count = %d/%d, want 1/1; history=%#v", repairedCalls, repairedResults, repaired)
	}
	if projectionIndex < 0 || projectionIndex+1 >= len(repaired) {
		t.Fatalf("projected call position %d leaves no room for its result", projectionIndex)
	}
	following := runreq.ExtractToolResultParts(repaired[projectionIndex+1])
	if len(following) != 1 || following[0].ToolCallID != "ask-1" {
		t.Fatalf("message after projection = %#v, want the genuine ask-1 result before narration", repaired[projectionIndex+1])
	}
}

func TestWebSendPersistsACPApprovalProjectionOnce(t *testing.T) {
	t.Parallel()

	streamed := []event.StreamEvent{
		{
			Type:       event.ToolCallStart,
			ToolCallID: "write-1",
			ToolName:   "write",
			Input:      map[string]any{"path": "/data/review.txt"},
		},
		{
			Type:       event.ToolApprovalRequest,
			ToolCallID: "write-1",
			ToolName:   "write",
			ApprovalID: "approval-1",
			ShortID:    3,
			Status:     toolapproval.StatusPending,
			Input:      map[string]any{"path": "/data/review.txt"},
		},
		{
			Type:       event.ToolApprovalRequest,
			ToolCallID: "write-1",
			ToolName:   "write",
			ApprovalID: "approval-1",
			ShortID:    3,
			Status:     toolapproval.StatusApproved,
			Input:      map[string]any{"path": "/data/review.txt"},
		},
		{
			Type:       event.ToolCallEnd,
			ToolCallID: "write-1",
			ToolName:   "write",
			Result:     map[string]any{"ok": true},
		},
		{Type: event.TextDelta, Delta: "done"},
	}
	messages := &agenttest.RecordingMessages{}
	pool := &recordingACPPrompter{
		result:       withTranscriptOutput(acpclient.PromptResult{}, streamed...),
		streamEvents: streamed,
	}
	resolver := newTestService(t, Deps{
		Messages:       messages,
		BotPermissions: allowWorkspaceExecFor("user-1"),
		Sessions: &agenttest.Sessions{
			GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
				return session.Thread{
					ID:    sessionID,
					BotID: "bot-1",
					Type:  session.TypeACPAgent,
					Metadata: map[string]any{
						"acp_agent_id":             "codex",
						"project_path":             "/data/app",
						"runtime_owner_account_id": "user-1",
					},
				}, nil
			},
			UpdateMetadataFn: func(_ context.Context, sessionID string, _ map[string]any) (session.Thread, error) {
				return session.Thread{ID: sessionID}, nil
			},
		},
	}, withWebAdmitter(), withACPPool(pool))

	if _, err := streamWebSend(context.Background(), t, resolver, runreq.ChatRequest{
		BotID:          "bot-1",
		ThreadID:       "session-1",
		Query:          "write the review",
		CurrentChannel: "web",
	}); err != nil {
		t.Fatalf("Web send failed: %v", err)
	}

	if len(messages.Persisted) != 5 {
		t.Fatalf("persisted %d messages, want user + temporary projection + complete ACP transcript", len(messages.Persisted))
	}
	pendingProjection := persistedModelMessage(t, messages.Persisted[1].Content)
	pendingCalls := runreq.ExtractAssistantToolCallParts(pendingProjection)
	if len(pendingCalls) != 1 || pendingCalls[0].ToolCallID != "write-1" {
		t.Fatalf("pending projected tool calls = %#v, want write-1", pendingCalls)
	}
	if got := toolCallMetadataStatus(pendingCalls[0], "approval"); got != toolapproval.StatusPending {
		t.Fatalf("pending projection status = %q, want pending", got)
	}
	terminalCall := persistedModelMessage(t, messages.Persisted[2].Content)
	terminalCalls := runreq.ExtractAssistantToolCallParts(terminalCall)
	if len(terminalCalls) != 1 || terminalCalls[0].ToolCallID != "write-1" {
		t.Fatalf("terminal approval tool calls = %#v, want write-1", terminalCalls)
	}
	if got := toolCallMetadataStatus(terminalCalls[0], "approval"); got != toolapproval.StatusApproved {
		t.Fatalf("terminal approval status = %q, want approved", got)
	}
	toolResult := persistedModelMessage(t, messages.Persisted[3].Content)
	results := runreq.ExtractToolResultParts(toolResult)
	if len(results) != 1 || results[0].ToolCallID != "write-1" {
		t.Fatalf("persisted approval tool result = %#v, want write-1", results)
	}
	final := persistedModelMessage(t, messages.Persisted[4].Content)
	if got := final.TextContent(); got != "done" {
		t.Fatalf("final assistant text = %q, want done", got)
	}
	if len(messages.Deleted) != 1 || len(messages.Deleted[0]) != 1 {
		t.Fatalf("temporary approval projection cleanup = %#v, want one deleted message", messages.Deleted)
	}
}

func TestStreamACPAgentWSRequestsAutoTitle(t *testing.T) {
	t.Parallel()

	sessionGets := make(chan string, 2)
	messages := &agenttest.RecordingMessages{}
	pool := &recordingACPPrompter{
		result: acpclient.PromptResult{
			Text:       "done",
			StopReason: "end_turn",
		},
	}
	resolver := newTestService(t, Deps{
		Messages:       messages,
		BotPermissions: allowWorkspaceExecFor("user-1"),
		Sessions: &agenttest.Sessions{
			GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
				recordSessionGet(sessionGets, sessionID)
				return session.Thread{
					ID:    sessionID,
					BotID: "bot-1",
					Type:  session.TypeACPAgent,
					Metadata: map[string]any{
						"acp_agent_id":             "codex",
						"project_path":             "/data/app",
						"runtime_owner_account_id": "user-1",
					},
				}, nil
			},
			UpdateMetadataFn: func(_ context.Context, sessionID string, _ map[string]any) (session.Thread, error) {
				return session.Thread{ID: sessionID}, nil
			},
		},
	}, withWebAdmitter(), withACPPool(pool))

	if _, err := streamExternalWebSend(
		context.Background(), resolver,
		runreq.ChatRequest{
			BotID:    "bot-1",
			ThreadID: "session-1",
			Query:    "inspect the app",
		},
		make(chan runreq.WSStreamEvent, 8),
		make(chan struct{}),
	); err != nil {
		t.Fatalf("streamACPAgentWS() error = %v", err)
	}

	if pool.input.SupportsImageInput {
		t.Fatalf("ACP prompt input SupportsImageInput = true, want false for read-media tool result decoration")
	}
	waitForSessionGets(t, sessionGets, 2)
}

func TestStreamACPAgentWSPropagatesContextBudgetDefaults(t *testing.T) {
	t.Parallel()

	const modelID = "00000000-0000-0000-0000-000000000301"
	const providerID = "00000000-0000-0000-0000-000000000302"
	provider := agenttest.ProviderRow(t, providerID, "openai-completions", true)
	model := agenttest.ModelRow(t, modelID, "gpt-context-window", provider.ID, models.ModelTypeChat, true)
	model.Config = []byte(`{"context_window": 128000}`)
	queries := &acpContextBudgetQueries{
		ModelQueries: &agenttest.ModelQueries{
			Models:   map[string]sqlc.Model{model.ModelID: model},
			Provider: provider,
		},
	}

	messages := &agenttest.RecordingMessages{}
	pool := &recordingACPPrompter{
		result: acpclient.PromptResult{Text: "done", StopReason: "end_turn"},
	}
	resolver := newTestService(t, Deps{
		Messages:       messages,
		BotPermissions: allowWorkspaceExecForBot(agenttest.MemoryBotID, "user-1"),
		Models:         models.NewService(slog.New(slog.DiscardHandler), queries),
		Queries:        queries,
		Settings:       settings.NewService(slog.New(slog.DiscardHandler), &acpContextBudgetSettingsQueries{chatModelID: modelID}, nil, nil),
		Sessions: &agenttest.Sessions{
			GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
				return session.Thread{
					ID:    sessionID,
					BotID: agenttest.MemoryBotID,
					Type:  session.TypeACPAgent,
					Metadata: map[string]any{
						"acp_agent_id":             "codex",
						"project_path":             "/data/app",
						"runtime_owner_account_id": "user-1",
					},
				}, nil
			},
			UpdateMetadataFn: func(_ context.Context, sessionID string, _ map[string]any) (session.Thread, error) {
				return session.Thread{ID: sessionID}, nil
			},
		},
	}, withWebAdmitter(), withACPPool(pool))

	if _, err := streamExternalWebSend(
		context.Background(), resolver,
		runreq.ChatRequest{
			BotID:    agenttest.MemoryBotID,
			ThreadID: "session-1",
			Query:    "inspect the app",
		},
		make(chan runreq.WSStreamEvent, 8),
		make(chan struct{}),
	); err != nil {
		t.Fatalf("streamACPAgentWS() error = %v", err)
	}

	if pool.input.ContextBudgetMaxTokens != 128000 {
		t.Fatalf("ContextBudgetMaxTokens = %d, want 128000", pool.input.ContextBudgetMaxTokens)
	}
}

type acpContextBudgetSettingsQueries struct {
	dbstore.Queries
	chatModelID string
}

type acpContextBudgetQueries struct {
	*agenttest.ModelQueries
}

func (*acpContextBudgetQueries) GetBotByID(context.Context, pgtype.UUID) (sqlc.GetBotByIDRow, error) {
	return sqlc.GetBotByIDRow{}, errors.New("bot timezone unavailable")
}

func (q *acpContextBudgetSettingsQueries) GetSettingsByBotID(_ context.Context, botID pgtype.UUID) (sqlc.GetSettingsByBotIDRow, error) {
	return sqlc.GetSettingsByBotIDRow{
		BotID:                   botID,
		ReasoningEffort:         "medium",
		CompactionTargetPercent: pgtype.Int4{Int32: 20, Valid: true},
		ChatModelID:             flowTestUUID(q.chatModelID),
	}, nil
}

func TestStreamACPAgentWSFailurePersistsRoundAndSkipsMemory(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	memory := &agenttest.MemoryProvider{AfterChat: make(chan memprovider.AfterChatRequest, 1)}
	registry := memprovider.NewRegistry(slog.New(slog.DiscardHandler))
	registry.Register(agenttest.MemoryProviderID, memory)
	pool := &recordingACPPrompter{err: errors.New("missing codex-acp")}
	resolver := newTestService(t, Deps{
		Messages:       messages,
		Settings:       settings.NewService(slog.New(slog.DiscardHandler), &agenttest.MemorySettingsQueries{}, nil, nil),
		BotPermissions: allowWorkspaceExecForBot(agenttest.MemoryBotID, "user-1"),
		Sessions: &agenttest.Sessions{
			GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
				return session.Thread{
					ID:    sessionID,
					BotID: agenttest.MemoryBotID,
					Type:  session.TypeACPAgent,
					Metadata: map[string]any{
						"acp_agent_id":             "codex",
						"project_path":             "/data/app",
						"runtime_owner_account_id": "user-1",
					},
				}, nil
			},
			UpdateMetadataFn: func(_ context.Context, sessionID string, _ map[string]any) (session.Thread, error) {
				return session.Thread{ID: sessionID}, nil
			},
		},
		Memory: registry,
	}, withWebAdmitter(), withACPPool(pool))

	eventCh := make(chan runreq.WSStreamEvent, 8)
	if _, err := streamExternalWebSend(
		context.Background(), resolver,
		runreq.ChatRequest{
			BotID:    agenttest.MemoryBotID,
			ThreadID: "session-1",
			Query:    "inspect",
		},
		eventCh,
		make(chan struct{}),
	); err != nil {
		t.Fatalf("streamACPAgentWS() error = %v", err)
	}

	if len(messages.Persisted) != 2 {
		t.Fatalf("persisted %d messages, want user + assistant", len(messages.Persisted))
	}
	if got := persistedText(t, messages.Persisted[1].Content); got != "" {
		t.Fatalf("assistant failure text = %q, want no text beside the failure code", got)
	}
	if _, exists := messages.Persisted[1].Metadata["error"]; exists {
		t.Fatalf("assistant metadata = %#v, want the failure recorded by its code alone", messages.Persisted[1].Metadata)
	}
	if got, _ := messages.Persisted[1].Metadata["error_code"].(string); got != "runtime_prompt_failed" {
		t.Fatalf("assistant error code metadata = %#v", messages.Persisted[1].Metadata)
	}
	if got := messages.Persisted[1].Metadata["agent_turn_outcome"]; got != "failed" {
		t.Fatalf("assistant failure outcome = %#v, want failed", got)
	}
	if publication := messages.RoundOptions[len(messages.RoundOptions)-1].AgentPublication; publication != nil {
		t.Fatalf("failed turn unexpectedly published head: %#v", publication)
	}
	events := drainAgentEvents(t, eventCh)
	if containsStreamEvent(events, native.EventTextDelta) {
		t.Fatalf("events = %#v, want the failure sent only as its error event", events)
	}
	if failure := requireStreamEvent(t, events, native.EventError); failure.Code != "runtime_prompt_failed" {
		t.Fatalf("failure event = %#v, want runtime_prompt_failed", failure)
	}
	abort := requireStreamEvent(t, events, native.EventAbort)
	var terminalMessages []runreq.ModelMessage
	if err := json.Unmarshal(abort.Messages, &terminalMessages); err != nil || len(terminalMessages) != 0 {
		t.Fatalf("terminal abort messages = %s (%v), want none", abort.Messages, err)
	}
	select {
	case got := <-memory.AfterChat:
		t.Fatalf("memory was called for ACP stream despite SkipMemory=true: %#v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

// A user's Stop keeps what the runtime had produced: the user message and the
// partial answer are in history, with no failure on either and no memory
// extracted from the turn.
func TestStreamACPAgentWSUserStopRecordsNoFailure(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	memory := &agenttest.MemoryProvider{AfterChat: make(chan memprovider.AfterChatRequest, 1)}
	registry := memprovider.NewRegistry(slog.New(slog.DiscardHandler))
	registry.Register(agenttest.MemoryProviderID, memory)
	started := make(chan struct{})
	pool := &recordingACPPrompter{
		promptFn: func(ctx context.Context, _ acpagent.PromptInput) (acpclient.PromptResult, error) {
			close(started)
			<-ctx.Done()
			return withTranscriptOutput(acpclient.PromptResult{}, event.StreamEvent{Type: event.TextDelta, Delta: "partial answer"}), context.Canceled
		},
	}
	resolver := newTestService(t, Deps{
		Messages:       messages,
		Settings:       settings.NewService(slog.New(slog.DiscardHandler), &agenttest.MemorySettingsQueries{}, nil, nil),
		BotPermissions: allowWorkspaceExecForBot(agenttest.MemoryBotID, "user-1"),
		Sessions: &agenttest.Sessions{
			GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
				return session.Thread{
					ID: sessionID, BotID: agenttest.MemoryBotID, Type: session.TypeACPAgent,
					Metadata: map[string]any{
						"acp_agent_id": "codex", "project_path": "/data/app",
						"runtime_owner_account_id": "user-1",
					},
				}, nil
			},
			UpdateMetadataFn: func(_ context.Context, sessionID string, _ map[string]any) (session.Thread, error) {
				return session.Thread{ID: sessionID}, nil
			},
		},
		Memory: registry,
	}, withWebAdmitter(), withACPPool(pool))

	abortCh := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ignoreOutcome(streamExternalWebSend(context.Background(), resolver, runreq.ChatRequest{
			BotID: agenttest.MemoryBotID, ThreadID: "session-1", Query: "inspect",
		}, make(chan runreq.WSStreamEvent, 8), abortCh))
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("ACP prompt did not start")
	}
	close(abortCh)
	if err := <-done; err != nil {
		t.Fatalf("streamACPAgentWS() error = %v", err)
	}

	if len(messages.Persisted) != 2 {
		t.Fatalf("persisted %d messages, want user + partial assistant", len(messages.Persisted))
	}
	if got := persistedText(t, messages.Persisted[1].Content); got != "partial answer" {
		t.Fatalf("assistant text = %q, want the partial answer", got)
	}
	for _, row := range messages.Persisted {
		if _, exists := row.Metadata["error"]; exists {
			t.Fatalf("stopped run persisted a failure marker: %#v", row.Metadata)
		}
		if _, exists := row.Metadata["error_code"]; exists {
			t.Fatalf("stopped run persisted a failure code: %#v", row.Metadata)
		}
	}
	select {
	case got := <-memory.AfterChat:
		t.Fatalf("memory extraction ran on a stopped turn: %#v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

// A prompt can complete in the same instant the user stops it. Stop wins: the
// output is kept, but the run ends with EventAbort instead of EventEnd and
// extracts no memory, so a stop is never recorded as a completed turn.
func TestStreamACPAgentWSStopRacingCompletionEndsAsAbort(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	memory := &agenttest.MemoryProvider{AfterChat: make(chan memprovider.AfterChatRequest, 1)}
	registry := memprovider.NewRegistry(slog.New(slog.DiscardHandler))
	registry.Register(agenttest.MemoryProviderID, memory)
	started := make(chan struct{})
	pool := &recordingACPPrompter{
		promptFn: func(ctx context.Context, _ acpagent.PromptInput) (acpclient.PromptResult, error) {
			close(started)
			<-ctx.Done()
			// The prompt finishes in the instant the run is stopped.
			return withTranscriptOutput(acpclient.PromptResult{}, event.StreamEvent{Type: event.TextDelta, Delta: "done"}), nil
		},
	}
	resolver := newTestService(t, Deps{
		Messages:       messages,
		Settings:       settings.NewService(slog.New(slog.DiscardHandler), &agenttest.MemorySettingsQueries{}, nil, nil),
		BotPermissions: allowWorkspaceExecForBot(agenttest.MemoryBotID, "user-1"),
		Sessions: &agenttest.Sessions{
			GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
				return session.Thread{
					ID: sessionID, BotID: agenttest.MemoryBotID, Type: session.TypeACPAgent,
					Metadata: map[string]any{
						"acp_agent_id": "codex", "project_path": "/data/app",
						"runtime_owner_account_id": "user-1",
					},
				}, nil
			},
			UpdateMetadataFn: func(_ context.Context, sessionID string, _ map[string]any) (session.Thread, error) {
				return session.Thread{ID: sessionID}, nil
			},
		},
		Memory: registry,
	}, withWebAdmitter(), withACPPool(pool))

	abortCh := make(chan struct{})
	eventCh := make(chan runreq.WSStreamEvent, 8)
	done := make(chan error, 1)
	go func() {
		done <- ignoreOutcome(streamExternalWebSend(context.Background(), resolver, runreq.ChatRequest{
			BotID: agenttest.MemoryBotID, ThreadID: "session-1", Query: "inspect",
		}, eventCh, abortCh))
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("ACP prompt did not start")
	}
	close(abortCh)
	if err := <-done; err != nil {
		t.Fatalf("streamACPAgentWS() error = %v", err)
	}

	if len(messages.Persisted) != 2 {
		t.Fatalf("persisted %d messages, want user + assistant output", len(messages.Persisted))
	}
	if got := persistedText(t, messages.Persisted[1].Content); got != "done" {
		t.Fatalf("assistant text = %q, want completed output kept", got)
	}
	for _, row := range messages.Persisted {
		if _, exists := row.Metadata["error"]; exists {
			t.Fatalf("stopped run persisted a failure marker: %#v", row.Metadata)
		}
	}
	events := drainAgentEvents(t, eventCh)
	if !containsStreamEvent(events, native.EventAbort) || containsStreamEvent(events, native.EventEnd) {
		t.Fatalf("events = %#v, want abort terminal without end", events)
	}
	select {
	case got := <-memory.AfterChat:
		t.Fatalf("memory extraction ran on a stopped turn: %#v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestStreamACPAgentWSExternalAgentErrorCarriesItsCode(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	pool := &recordingACPPrompter{err: fmt.Errorf("start: %w", acpagent.ErrAgentNotConfigured)}
	lifecycles := &agenttest.RecordingLifecycleStore{}
	resolver := newACPLifecycleService(t, pool, messages, lifecycles)

	eventCh := make(chan runreq.WSStreamEvent, 8)
	runResult, err := streamExternalWebSend(
		context.Background(), resolver,
		runreq.ChatRequest{
			BotID:    agenttest.LifecycleBotID,
			ThreadID: agenttest.LifecycleSessionID,
			RunID:    agenttest.LifecycleRunID,
			Query:    "inspect",
		},
		eventCh,
		make(chan struct{}),
	)
	if err != nil || runResult.ErrorCode() != string(apperror.CodeACPAgentNotConfigured) {
		t.Fatalf("streamACPAgentWS() = %q, %v, want %s", runResult.ErrorCode(), err, apperror.CodeACPAgentNotConfigured)
	}
	requireACPLifecycle(t, lifecycles, agenttest.LifecycleRunID, lifecycle.StatusFailedProvider)
	events := drainAgentEvents(t, eventCh)
	if !containsStreamEvent(events, native.EventError) || !containsStreamEvent(events, native.EventAbort) {
		t.Fatalf("events = %#v, want the failure frame and an aborted terminal", events)
	}
}

func TestStreamACPAgentWSImageCapabilityErrorCarriesItsCode(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	resolver := newTestService(t, Deps{
		Messages:       messages,
		BotPermissions: allowWorkspaceExecFor("user-1"),
		Sessions: &agenttest.Sessions{
			GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
				return session.Thread{
					ID:    sessionID,
					BotID: "bot-1",
					Type:  session.TypeACPAgent,
					Metadata: map[string]any{
						"acp_agent_id":             "codex",
						"project_path":             "/data/app",
						"runtime_owner_account_id": "user-1",
					},
				}, nil
			},
			UpdateMetadataFn: func(_ context.Context, sessionID string, _ map[string]any) (session.Thread, error) {
				return session.Thread{ID: sessionID}, nil
			},
		},
	}, withWebAdmitter(), withACPPool(&recordingACPPrompter{err: acpclient.ErrImagePromptUnsupported}))

	runResult, err := streamExternalWebSend(
		context.Background(), resolver,
		runreq.ChatRequest{
			BotID:    "bot-1",
			ThreadID: "session-1",
			Query:    "inspect",
			Attachments: []runreq.ChatAttachment{{
				Type:   "image",
				Name:   "screen.png",
				Mime:   "image/png",
				Base64: agenttest.DataURL("image/png", agenttest.RasterPNG(t)),
			}},
		},
		make(chan runreq.WSStreamEvent, 8),
		make(chan struct{}),
	)
	if err != nil || runResult.ErrorCode() != string(apperror.CodeACPImageInputUnsupported) {
		t.Fatalf("streamACPAgentWS() = %q, %#v, want %s", runResult.ErrorCode(), err, apperror.CodeACPImageInputUnsupported)
	}
}

func TestStreamACPAgentWSSuccessStoresMemory(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	memory := &agenttest.MemoryProvider{
		BeforeChat: &memprovider.BeforeChatResult{
			ContextText:   "remembered fact",
			RetrievalMode: "graph",
			ResultCount:   1,
			ResultRefs:    []string{"memory-1"},
		},
		AfterChat: make(chan memprovider.AfterChatRequest, 1),
	}
	registry := memprovider.NewRegistry(slog.New(slog.DiscardHandler))
	registry.Register(agenttest.MemoryProviderID, memory)
	pool := &recordingACPPrompter{
		result: withTranscriptOutput(acpclient.PromptResult{}, event.StreamEvent{Type: event.TextDelta, Delta: "done"}),
	}
	resolver := newTestService(t, Deps{
		Messages:       messages,
		Settings:       settings.NewService(slog.New(slog.DiscardHandler), &agenttest.MemorySettingsQueries{}, nil, nil),
		BotPermissions: allowWorkspaceExecForBot(agenttest.MemoryBotID, "user-1"),
		Sessions: &agenttest.Sessions{
			GetFn: func(_ context.Context, sessionID string) (session.Thread, error) {
				return session.Thread{
					ID:    sessionID,
					BotID: agenttest.MemoryBotID,
					Type:  session.TypeACPAgent,
					Metadata: map[string]any{
						"acp_agent_id":             "codex",
						"project_path":             "/data/app",
						"runtime_owner_account_id": "user-1",
					},
				}, nil
			},
			UpdateMetadataFn: func(_ context.Context, sessionID string, _ map[string]any) (session.Thread, error) {
				return session.Thread{ID: sessionID}, nil
			},
		},
		Memory: registry,
	}, withWebAdmitter(), withACPPool(pool))

	if _, err := streamExternalWebSend(
		context.Background(), resolver,
		runreq.ChatRequest{
			BotID:    agenttest.MemoryBotID,
			ThreadID: "session-1",
			Query:    "inspect",
		},
		make(chan runreq.WSStreamEvent, 8),
		make(chan struct{}),
	); err != nil {
		t.Fatalf("streamACPAgentWS() error = %v", err)
	}

	select {
	case got := <-memory.AfterChat:
		if len(got.Messages) != 2 {
			t.Fatalf("memory messages = %#v, want user + assistant", got.Messages)
		}
		if got.Messages[0].Role != "user" || got.Messages[0].Content != "inspect" {
			t.Fatalf("memory user message = %#v", got.Messages[0])
		}
		if got.Messages[1].Role != "assistant" || got.Messages[1].Content != "done" {
			t.Fatalf("memory assistant message = %#v", got.Messages[1])
		}
	case <-time.After(time.Second):
		t.Fatal("memory was not called for successful ACP stream")
	}

	var snapshot *contextfrag.LifecycleSnapshot
	for i := len(messages.Persisted) - 1; i >= 0; i-- {
		if messages.Persisted[i].Role != "assistant" {
			continue
		}
		value, ok := messages.Persisted[i].Metadata[contextfrag.MetadataContextLifecycleKey].(contextfrag.LifecycleSnapshot)
		if ok {
			snapshot = &value
		}
		break
	}
	if snapshot == nil || snapshot.MemoryRecall == nil {
		t.Fatalf("ACP lifecycle missing memory trace: %#v", snapshot)
	}
	if snapshot.MemoryRecall.ProviderID != agenttest.MemoryProviderID || snapshot.MemoryRecall.CacheState != "miss" ||
		snapshot.MemoryRecall.Result.Count != 1 || !slices.Equal(snapshot.MemoryRecall.Result.Refs, []string{"memory-1"}) {
		t.Fatalf("ACP memory trace = %#v", snapshot.MemoryRecall)
	}
}

func allowWorkspaceExecFor(accountID string) *agenttest.PermissionChecker {
	return allowWorkspaceExecForBot("bot-1", accountID)
}

func requireStreamEvent(t *testing.T, events []native.StreamEvent, eventType native.StreamEventType) native.StreamEvent {
	t.Helper()
	for _, event := range events {
		if event.Type == eventType {
			return event
		}
	}
	t.Fatalf("events = %#v, want %s", events, eventType)
	return native.StreamEvent{}
}

func containsTextDelta(events []native.StreamEvent, delta string) bool {
	for _, event := range events {
		if event.Type == native.EventTextDelta && event.Delta == delta {
			return true
		}
	}
	return false
}

func recordSessionGet(ch chan<- string, sessionID string) {
	select {
	case ch <- sessionID:
	default:
	}
}

func waitForSessionGets(t *testing.T, ch <-chan string, want int) {
	t.Helper()
	deadline := time.After(time.Second)
	for count := 0; count < want; count++ {
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("observed %d session Get calls, want %d", count, want)
		}
	}
}

func toolCallMetadataStatus(call sdk.ToolCallPart, key string) string {
	raw, ok := partmeta.Object(call.ProviderMetadata, key)
	if !ok {
		return ""
	}
	status, _ := raw["status"].(string)
	return status
}

func withTranscriptOutput(result acpclient.PromptResult, events ...event.StreamEvent) acpclient.PromptResult {
	result.Output = acpclient.TranscriptFromEvents(events, result.Text)
	return result
}

// withWebAdmission replaces the session runtime with one that admits every Web
// send as the run webTestAdmission names for req.
func withWebAdmission(req runreq.ChatRequest) testOption {
	return withTurnAdmitter(&agenttest.LifecycleAdmitter{Admission: webTestAdmission(req)})
}
