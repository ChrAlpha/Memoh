package agent

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/contextview"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// ---------------------------------------------------------------- END ADAPTER

// planCapture records the RunConfig the agent receives at Stream time.
type planCapture struct {
	mu   sync.Mutex
	cfgs []native.RunConfig
}

func (c *planCapture) apply(ctx context.Context, cfg native.RunConfig) native.RunConfig {
	c.mu.Lock()
	c.cfgs = append(c.cfgs, cfg)
	c.mu.Unlock()
	return contextview.ProviderRunConfigApplier(slog.New(slog.DiscardHandler))(ctx, cfg)
}

func (c *planCapture) first(t *testing.T) native.RunConfig {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cfgs) == 0 {
		t.Fatal("agent.Stream was never reached; no RunConfig captured")
	}
	return c.cfgs[0]
}

// ---- E. continuation chain ------------------------------------------------

const (
	planApprovalToolName = "plan_edit"
	planUIDiff           = "--- a/file\n+++ b/file\n@@ -1 +1 @@\n-old\n+new\n"
)

// planTool is a tool whose output carries a UI-only payload next to its
// model-visible result, and which records the arguments it was called with.
type planTool struct {
	mu     sync.Mutex
	inputs []map[string]any
}

func (p *planTool) Tools(context.Context, tools.SessionContext) ([]toolexec.Tool, error) {
	return []toolexec.Tool{{
		Name: planApprovalToolName,
		Execute: func(_ *toolexec.ToolExecContext, input sdk.ToolArguments) (sdk.ToolOutput, error) {
			var args map[string]any
			if err := input.Unmarshal(&args); err != nil {
				return sdk.ToolOutput{}, err
			}
			p.mu.Lock()
			p.inputs = append(p.inputs, args)
			p.mu.Unlock()
			return toolexec.OutputFromValue(map[string]any{
				"status":                  "edited",
				tools.UIOutputMetadataKey: map[string]any{"diff": planUIDiff},
			}), nil
		},
	}}, nil
}

func (p *planTool) calls() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.inputs...)
}

// decisionQueries answers the fenced transaction an approved tool's execution
// checkpoints in, and records the checkpoint.
type decisionQueries struct {
	dbstore.Queries
	mu      sync.Mutex
	params  []sqlc.MarkSessionRunDecisionExecutingParams
	lineage []pgtype.UUID
	count   int64
}

func (q *decisionQueries) InTx(_ context.Context, fn func(dbstore.Queries) error) error { return fn(q) }

func (*decisionQueries) SupportsTransactions() bool { return true }

func (*decisionQueries) LockBotForSessionWrite(_ context.Context, id pgtype.UUID) (pgtype.UUID, error) {
	return id, nil
}

func (*decisionQueries) LockSessionRuntimeFence(context.Context, sqlc.LockSessionRuntimeFenceParams) (int64, error) {
	return 1, nil
}

func (q *decisionQueries) MarkSessionRunDecisionExecuting(_ context.Context, arg sqlc.MarkSessionRunDecisionExecutingParams) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.params = append(q.params, arg)
	return q.count, nil
}

func (q *decisionQueries) ListCompactionArtifactLineageBySession(ctx context.Context, sessionID pgtype.UUID) ([]sqlc.BotHistoryMessageCompact, error) {
	q.mu.Lock()
	q.lineage = append(q.lineage, sessionID)
	q.mu.Unlock()
	return q.Queries.ListCompactionArtifactLineageBySession(ctx, sessionID)
}

func (q *decisionQueries) lineageSessions() []pgtype.UUID {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]pgtype.UUID(nil), q.lineage...)
}

func (q *decisionQueries) recorded() []sqlc.MarkSessionRunDecisionExecutingParams {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]sqlc.MarkSessionRunDecisionExecutingParams(nil), q.params...)
}

// decisionHistory is a session history that also names the request message of
// the turn a decision parked, which is what a continuation resumes from.
type decisionHistory struct {
	*driveHistoryMessages
	requestID string
	err       error
}

func (m *decisionHistory) GetRunTurnRequest(context.Context, string, string, string) (messagepkg.RunTurnRequest, error) {
	return messagepkg.RunTurnRequest{MessageID: m.requestID}, m.err
}

type decisionHarness struct {
	service *Service
	history *decisionHistory
	capture *planCapture
	tool    *planTool
	queries *decisionQueries
}

func toolRows(h decisionHarness) []messagepkg.PersistInput {
	var rows []messagepkg.PersistInput
	for _, input := range h.history.Persisted {
		if input.Role == "tool" {
			rows = append(rows, input)
		}
	}
	return rows
}
