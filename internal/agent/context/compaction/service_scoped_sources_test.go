package compaction

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

func TestScopedCompactionDoesNotRechargeFixedContext(t *testing.T) {
	for _, mode := range []string{"latest_tool", "unseen_history", "unseen_summary", "eligible_tail", "eligible_summary"} {
		t.Run(mode, func(t *testing.T) {
			const summary = "summary: "
			text := summary + strings.Repeat("s", 800)
			var calls, outputLimit atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					MaxTokens           int `json:"max_tokens"`
					MaxCompletionTokens int `json:"max_completion_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				calls.Add(1)
				limit := max(request.MaxTokens, request.MaxCompletionTokens)
				outputLimit.Store(int64(limit))
				finishReason := "stop"
				if limit < 210 {
					finishReason = "length"
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "summary", "object": "chat.completion", "model": "scripted",
					"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "finish_reason": finishReason}},
				})
			}))
			defer server.Close()
			eligible := mkRow(t, "assistant", jsonStr(strings.Repeat("completed step ", 500)), 2000)
			rows := []sqlc.ListUncompactedMessagesBySessionRow{eligible}
			switch mode {
			case "latest_tool":
				call, result := toolCallRow(t, 100), toolResultRow(t, 400)
				result.Content = []byte(`[{"type":"tool-result","toolName":"calc","toolCallId":"c1","result":` + jsonStr(strings.Repeat("f", 1200)) + `}]`)
				rows = append(rows, call, result)
			case "unseen_history":
				rows = append([]sqlc.ListUncompactedMessagesBySessionRow{mkRow(t, "assistant", jsonStr(strings.Repeat("unseen", 200)), 400)}, rows...)
			}
			cfg := machineryConfig(nil, 500)
			cfg.BaseURL, cfg.HTTPClient = server.URL, server.Client()
			cfg.HistoryBudgetTokens = 600
			cfg.EligibleSources = []contextfrag.ContextRef{scopedRowRef(t, eligible)}
			q := &fakeQueries{uncompacted: rows}
			if mode == "unseen_summary" || mode == "eligible_summary" {
				q.priorLogs = fusionParentLogs(t, cfg, strings.Repeat("unseen", 200))
				if mode == "eligible_summary" {
					cfg.EligibleSources = append(cfg.EligibleSources, fusionArtifacts(t, q.priorLogs)[0].Coverage[0].Ref)
				}
			}
			if mode == "eligible_tail" {
				tail := mkRow(t, "assistant", jsonStr(strings.Repeat("retained", 150)), 400)
				q.uncompacted = append(q.uncompacted, mkRow(t, "user", `"protected barrier"`, 10), tail)
				cfg.EligibleSources = append(cfg.EligibleSources, scopedRowRef(t, tail))
			}
			result, err := newMachineryService(q).RunCompactionSync(t.Context(), cfg)
			t.Logf("scope_budget=%d target=%d max_tokens=%d provider_calls=%d", cfg.HistoryBudgetTokens, cfg.TargetTokens, outputLimit.Load(), calls.Load())
			if mode == "eligible_tail" || mode == "eligible_summary" {
				if err == nil || calls.Load() != 1 || q.completed.Status != "error" || len(q.markedIDs) != 1 || q.markedIDs[0] != eligible.ID {
					t.Fatalf("eligible retained context was not budgeted: result=%+v err=%v calls=%d max_tokens=%d", result, err, calls.Load(), outputLimit.Load())
				}
				return
			}
			if err != nil || result.Status != StatusOK || calls.Load() != 1 || outputLimit.Load() < 210 {
				t.Fatalf("fixed context charged twice: result=%+v err=%v calls=%d max_tokens=%d", result, err, calls.Load(), outputLimit.Load())
			}
			if len(q.markedIDs) != 1 || q.markedIDs[0] != eligible.ID || q.completed.Status != "ok" {
				t.Fatalf("scope or commit changed: marked=%v status=%s", q.markedIDs, q.completed.Status)
			}
		})
	}
}

func TestScopedCompactionReplacesCommittedMiddleBehindCurrentInput(t *testing.T) {
	rows := []sqlc.ListUncompactedMessagesBySessionRow{
		mkRow(t, "user", `"current instruction"`, 100),
		mkRow(t, "assistant", jsonStr(strings.Repeat("completed earlier step ", 80)), 1000),
		toolCallRow(t, 100),
		toolResultRow(t, 100),
	}
	stub := &stubModel{summary: "earlier step summarized"}
	cfg := machineryConfig(stub, 200)
	cfg.ProtectedSources = []turn.ContextMessageSource{{Kind: "history", ID: rows[0].ID.String(), Current: true}}
	cfg.EligibleSources = []contextfrag.ContextRef{scopedRowRef(t, rows[1])}
	q := &fakeQueries{uncompacted: rows}
	result, err := newMachineryService(q).RunCompactionSync(t.Context(), cfg)
	if err != nil || result.Status != StatusOK || len(q.markedIDs) != 1 || q.markedIDs[0] != rows[1].ID {
		t.Fatalf("committed middle did not compact: result=%+v err=%v marked=%v", result, err, q.markedIDs)
	}
	coverage, err := DecodeArtifactCoverage(q.completed.Coverage)
	if err != nil || len(coverage) != 1 || coverage[0].Ref != cfg.EligibleSources[0] {
		t.Fatalf("coverage = %+v, err=%v", coverage, err)
	}
}

func TestScopedCompactionMatchesAttachmentAwareSourceHash(t *testing.T) {
	rows := qualityRows(t)
	asset := sqlc.ListMessageAssetsBatchRow{MessageID: rows[0].ID, ContentHash: "asset-hash", Role: "attachment", Name: "diagram.png"}
	message := rowToMessage(rows[0])
	message.Assets = []messagepkg.MessageAsset{{ContentHash: asset.ContentHash, Role: asset.Role, Name: asset.Name}}
	record, err := historyfrag.FromDBMessage(message, rowScopeFallback(rows[0]))
	if err != nil {
		t.Fatal(err)
	}
	stub := &stubModel{summary: "earlier attachment summarized"}
	cfg := machineryConfig(stub, 200)
	cfg.EligibleSources = []contextfrag.ContextRef{record.Ref}
	q := &artifactQueries{fakeQueries: &fakeQueries{uncompacted: rows}, assets: []sqlc.ListMessageAssetsBatchRow{asset}}
	result, err := newMachineryService(q).RunCompactionSync(t.Context(), cfg)
	if err != nil || result.Status != StatusOK || len(q.markedIDs) != 1 || q.markedIDs[0] != rows[0].ID {
		t.Fatalf("attachment source did not compact: result=%+v err=%v marked=%v", result, err, q.markedIDs)
	}
	coverage, err := DecodeArtifactCoverage(q.completed.Coverage)
	if err != nil || len(coverage) != 1 || coverage[0].Ref != record.Ref {
		t.Fatalf("attachment coverage=%+v err=%v", coverage, err)
	}
}

func TestScopedCompactionKeepsPartiallyEligibleFrontierAndItsBudget(t *testing.T) {
	stub := &stubModel{summary: "earlier work summarized"}
	cfg := machineryConfig(stub, 800)
	cfg.AllowFrontierFusion = true
	cfg.ContextWindowTokens = 2000
	cfg.HistoryBudgetTokens = 900
	parents := fusionParentLogs(t, cfg, strings.Repeat("allowed ", 500), strings.Repeat("retained ", 500))
	artifacts := fusionArtifacts(t, parents)
	rows := fusionQualityRows(t, cfg)
	cfg.EligibleSources = []contextfrag.ContextRef{artifacts[0].Coverage[0].Ref, scopedRowRef(t, rows[0]), scopedRowRef(t, rows[1])}
	q := &fakeQueries{uncompacted: rows, priorLogs: parents}
	result, err := newMachineryService(q).RunCompactionSync(t.Context(), cfg)
	if err != nil || len(q.rollupCalls) != 0 || len(q.markedIDs) != 2 {
		t.Fatalf("raw scoped compaction failed: result=%+v err=%v parents=%v marked=%v", result, err, q.rollupCompleted.Parents, q.markedIDs)
	}
	if len(q.parentSuccessors) != 0 || result.Status != StatusProgress {
		t.Fatalf("retained frontier lost or omitted from budget: result=%+v successors=%v", result, q.parentSuccessors)
	}
	coverage, err := DecodeArtifactCoverage(q.completed.Coverage)
	if err != nil || len(coverage) != 2 || coverage[0].Ref != cfg.EligibleSources[1] || coverage[1].Ref != cfg.EligibleSources[2] {
		t.Fatalf("raw coverage=%+v err=%v", coverage, err)
	}
}

func TestScopedCompactionRejectsIncompleteToolGroup(t *testing.T) {
	call := toolCallRow(t, 100)
	q := &fakeQueries{uncompacted: []sqlc.ListUncompactedMessagesBySessionRow{call}}
	stub := &stubModel{summary: "s"}
	cfg := machineryConfig(stub, 1)
	cfg.EligibleSources = []contextfrag.ContextRef{scopedRowRef(t, call)}
	result, err := newMachineryService(q).RunCompactionSync(t.Context(), cfg)
	if err != nil || result.Status != StatusNoop || stub.calls != 0 || q.created {
		t.Fatalf("incomplete tool group compacted: result=%+v err=%v calls=%d created=%v", result, err, stub.calls, q.created)
	}
}

type scopedChangingAssetQueries struct{ *artifactQueries }

func (q scopedChangingAssetQueries) ListMessageAssetsBatch(ctx context.Context, ids []pgtype.UUID) ([]sqlc.ListMessageAssetsBatchRow, error) {
	assets, err := q.artifactQueries.ListMessageAssetsBatch(ctx, ids)
	if q.assetCalls > 1 {
		assets = append([]sqlc.ListMessageAssetsBatchRow(nil), assets...)
		assets[0].ContentHash = "changed-asset"
	}
	return assets, err
}

func TestScopedCompactionRejectsSourceChangedAfterClaim(t *testing.T) {
	rows := qualityRows(t)
	asset := sqlc.ListMessageAssetsBatchRow{MessageID: rows[0].ID, ContentHash: "original-asset", Role: "attachment"}
	message := rowToMessage(rows[0])
	message.Assets = []messagepkg.MessageAsset{{ContentHash: asset.ContentHash, Role: asset.Role}}
	record, err := historyfrag.FromDBMessage(message, rowScopeFallback(rows[0]))
	if err != nil {
		t.Fatal(err)
	}
	stub := &stubModel{summary: "short summary"}
	cfg := machineryConfig(stub, 200)
	cfg.EligibleSources = []contextfrag.ContextRef{record.Ref}
	q := scopedChangingAssetQueries{&artifactQueries{fakeQueries: &fakeQueries{uncompacted: rows}, assets: []sqlc.ListMessageAssetsBatchRow{asset}}}
	result, err := newMachineryService(q).RunCompactionSync(t.Context(), cfg)
	if err == nil || result.Status == StatusOK || stub.calls != 0 || q.completed.Status != "error" {
		t.Fatalf("changed scoped source accepted: result=%+v err=%v calls=%d status=%s", result, err, stub.calls, q.completed.Status)
	}
}

func TestScopedCompactionRequiresExactEligibleSources(t *testing.T) {
	for _, mode := range []string{"nil", "empty", "hash", "identity", "missing_hash", "protected"} {
		t.Run(mode, func(t *testing.T) {
			rows := qualityRows(t)
			stub := &stubModel{summary: "s"}
			cfg := machineryConfig(stub, 200)
			cfg.EligibleSources = []contextfrag.ContextRef{scopedRowRef(t, rows[0])}
			switch mode {
			case "nil":
				cfg.EligibleSources = nil
			case "empty":
				cfg.EligibleSources = []contextfrag.ContextRef{}
			case "hash":
				cfg.EligibleSources[0].ContentHash = strings.Repeat("0", 64)
			case "identity":
				cfg.EligibleSources[0].Version++
			case "missing_hash":
				cfg.EligibleSources[0].ContentHash = ""
			case "protected":
				cfg.ProtectedSources = []turn.ContextMessageSource{{Kind: "history", ID: rows[0].ID.String()}}
			}
			q := &fakeQueries{uncompacted: rows}
			result, err := newMachineryService(q).RunCompactionSync(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "nil" {
				if result.Status != StatusOK || len(q.markedIDs) != 2 {
					t.Fatalf("ordinary selection changed: result=%+v marked=%v", result, q.markedIDs)
				}
			} else if result.Status != StatusNoop || stub.calls != 0 || q.created {
				t.Fatalf("unpermitted source compacted: result=%+v calls=%d created=%v", result, stub.calls, q.created)
			}
		})
	}
}

func TestScopedCompactionKeepsToolGroupAtomic(t *testing.T) {
	for _, mode := range []string{"complete", "missing_call", "missing_result", "wrong_result", "ask_user"} {
		t.Run(mode, func(t *testing.T) {
			rows := []sqlc.ListUncompactedMessagesBySessionRow{toolCallRow(t, 100), toolResultRow(t, 100)}
			if mode == "wrong_result" {
				rows[1].Content = []byte(strings.ReplaceAll(string(rows[1].Content), "c1", "different"))
			}
			if mode == "ask_user" {
				for i := range rows {
					rows[i].Content = []byte(strings.ReplaceAll(string(rows[i].Content), "calc", "ask_user"))
				}
			}
			stub := &stubModel{summary: "s"}
			cfg := machineryConfig(stub, 1)
			cfg.EligibleSources = []contextfrag.ContextRef{scopedRowRef(t, rows[0]), scopedRowRef(t, rows[1])}
			if mode == "missing_call" {
				cfg.EligibleSources = cfg.EligibleSources[1:]
			}
			if mode == "missing_result" {
				cfg.EligibleSources = cfg.EligibleSources[:1]
			}
			q := &fakeQueries{uncompacted: rows}
			result, err := newMachineryService(q).RunCompactionSync(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "complete" {
				if result.Status != StatusOK || len(q.markedIDs) != 2 {
					t.Fatalf("complete group did not compact: result=%+v marked=%v", result, q.markedIDs)
				}
			} else if result.Status != StatusNoop || stub.calls != 0 || q.created {
				t.Fatalf("protected tool group compacted: result=%+v calls=%d created=%v", result, stub.calls, q.created)
			}
		})
	}
}

func TestScopedCompactionRequiresWholeFrontierCoverage(t *testing.T) {
	for _, mode := range []string{"complete", "empty", "partial", "hash", "protected_history", "protected_external", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			stub := &stubModel{summary: "short replacement"}
			cfg := machineryConfig(stub, 800)
			cfg.AllowFrontierFusion = true
			cfg.ContextWindowTokens = 2000
			parents := fusionParentLogs(t, cfg, strings.Repeat("a", 2040), strings.Repeat("b", 2040))
			coverage, err := DecodeArtifactCoverage(parents[0].Coverage)
			if err != nil {
				t.Fatal(err)
			}
			coverage[0].ExternalMessageID = "external-input"
			parents[0].Coverage = mustMarshalCoverage(t, coverage...)
			artifacts := fusionArtifacts(t, parents)
			cfg.EligibleSources = []contextfrag.ContextRef{artifacts[0].Coverage[0].Ref, artifacts[1].Coverage[0].Ref}
			switch mode {
			case "empty":
				cfg.EligibleSources = []contextfrag.ContextRef{}
			case "partial":
				cfg.EligibleSources = cfg.EligibleSources[:1]
			case "hash":
				cfg.EligibleSources[0].ContentHash = strings.Repeat("0", 64)
			case "protected_history":
				cfg.ProtectedSources = []turn.ContextMessageSource{{Kind: "history", ID: cfg.EligibleSources[0].ID}}
			case "protected_external":
				cfg.ProtectedSources = []turn.ContextMessageSource{{Kind: "external", ID: coverage[0].ExternalMessageID}}
			case "legacy":
				parents[0].Coverage = []byte("[]")
			}
			q := &fakeQueries{priorLogs: parents}
			result, err := newMachineryService(q).RunCompactionSync(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "complete" {
				if result.Status != StatusOK || len(q.rollupCompleted.Parents) != 2 {
					t.Fatalf("eligible frontier did not fuse: result=%+v parents=%v", result, q.rollupCompleted.Parents)
				}
			} else if result.Status != StatusNoop || stub.calls != 0 || q.created || len(q.parentSuccessors) != 0 {
				t.Fatalf("unpermitted frontier fused: result=%+v calls=%d successors=%v", result, stub.calls, q.parentSuccessors)
			}
		})
	}
}

func scopedRowRef(t *testing.T, row sqlc.ListUncompactedMessagesBySessionRow) contextfrag.ContextRef {
	t.Helper()
	record, err := historyfrag.FromDBMessage(rowToMessage(row), rowScopeFallback(row))
	if err != nil {
		t.Fatal(err)
	}
	return record.Ref
}
