package compaction

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/chat/checkpoint"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// --- stub summarizer model (intercepts the SDK HTTP call) ---------------------

type stubModel struct {
	summary      string
	finishReason string // defaults to "stop"
	calls        int
	prompt       string // decoded text of the captured request messages
	maxTokens    int    // captured max_tokens of the last request
	reasoning    int    // output tokens spent on reasoning before the summary text
	// during runs while the summary is written, before the pass commits it.
	during func()
}

func (s *stubModel) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls++
	if s.during != nil {
		s.during()
	}
	if req.Body != nil {
		body, _ := io.ReadAll(req.Body)
		s.prompt = decodePromptMessages(body)
		var limits struct {
			MaxTokens           int `json:"max_tokens"`
			MaxCompletionTokens int `json:"max_completion_tokens"`
		}
		_ = json.Unmarshal(body, &limits)
		s.maxTokens = max(limits.MaxTokens, limits.MaxCompletionTokens)
	}
	finishReason := s.finishReason
	if finishReason == "" {
		finishReason = "stop"
	}
	if s.reasoning > 0 && (s.maxTokens-s.reasoning)*4 < len(s.summary) {
		finishReason = "length"
	}
	resp := `{"id":"stub","object":"chat.completion","created":0,"model":"stub",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":` + jsonStr(s.summary) + `},"finish_reason":` + jsonStr(finishReason) + `}],` +
		`"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(resp)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func decodePromptMessages(body []byte) string {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &req)
	var sb strings.Builder
	for _, m := range req.Messages {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			sb.WriteString(s)
			sb.WriteByte('\n')
			continue
		}
		var parts []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &parts) == nil {
			for _, p := range parts {
				sb.WriteString(p.Text)
				sb.WriteByte('\n')
			}
		}
	}
	return sb.String()
}

// fakeQueries is one session's history, compaction logs and history
// checkpoints in memory, read and written as the SQL the compaction engine
// and the checkpoint store run reads and writes them. What InTx writes rolls
// back when its callback fails.
type fakeQueries struct {
	dbstore.Queries // embedded interface; unimplemented methods would panic if called

	// history is the session's context rows in history order. A row without
	// a turn position takes the next one when the fake first reads it.
	history     []candidateRow
	logs        map[pgtype.UUID]sqlc.BotHistoryMessageCompact
	checkpoints map[pgtype.UUID]sqlc.BotHistoryCheckpoint
	active      pgtype.UUID
	// folded names the rows the newest checkpoint folds, in history order.
	folded []pgtype.UUID

	listPanic   bool
	listStarted chan struct{}
	listRelease <-chan struct{}
	assetsErr   error
	createErr   error
	completeErr error
	// completeErrors fail the next completions in turn; a nil one passes.
	completeErrors []error
	onComplete     func()
	// onRewind applies a rewind before the replaced turn; rewinds records
	// each one asked for.
	onRewind func()
	rewinds  []sqlc.RewindHistoryCheckpointBeforeTurnParams

	created       bool
	createdLogID  pgtype.UUID
	completed     sqlc.CompleteCompactionLogParams
	completeCalls []sqlc.CompleteCompactionLogParams
	queryCalls    []string
}

func (*fakeQueries) SupportsTransactions() bool { return true }

func (f *fakeQueries) InTx(_ context.Context, fn func(dbstore.Queries) error) error {
	logs, checkpoints, active, folded := maps.Clone(f.logs), maps.Clone(f.checkpoints), f.active, f.folded
	if err := fn(f); err != nil {
		f.logs, f.checkpoints, f.active, f.folded = logs, checkpoints, active, folded
		return err
	}
	return nil
}

func (*fakeQueries) LockBotForSessionWrite(_ context.Context, id pgtype.UUID) (pgtype.UUID, error) {
	return id, nil
}

func (*fakeQueries) ListSessionEventCursors(context.Context, sqlc.ListSessionEventCursorsParams) ([]sqlc.ListSessionEventCursorsRow, error) {
	return nil, nil
}

func (f *fakeQueries) RewindHistoryCheckpointBeforeTurn(_ context.Context, arg sqlc.RewindHistoryCheckpointBeforeTurnParams) (bool, error) {
	f.rewinds = append(f.rewinds, arg)
	if f.onRewind == nil {
		return false, nil
	}
	f.onRewind()
	return true, nil
}

func (f *fakeQueries) CreateCompactionLog(_ context.Context, arg sqlc.CreateCompactionLogParams) (sqlc.BotHistoryMessageCompact, error) {
	f.created = true
	if f.createErr != nil {
		return sqlc.BotHistoryMessageCompact{}, f.createErr
	}
	log := sqlc.BotHistoryMessageCompact{ID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, BotID: arg.BotID, SessionID: arg.SessionID, Status: "pending"}
	f.createdLogID = log.ID
	f.putLog(log)
	return log, nil
}

func (f *fakeQueries) CompleteCompactionLog(_ context.Context, arg sqlc.CompleteCompactionLogParams) (sqlc.BotHistoryMessageCompact, error) {
	f.completeCalls = append(f.completeCalls, arg)
	if f.onComplete != nil {
		f.onComplete()
	}
	if len(f.completeErrors) > 0 {
		err := f.completeErrors[0]
		f.completeErrors = f.completeErrors[1:]
		if err != nil {
			return sqlc.BotHistoryMessageCompact{}, err
		}
	}
	if f.completeErr != nil {
		return sqlc.BotHistoryMessageCompact{}, f.completeErr
	}
	f.completed = arg
	log := f.logs[arg.ID]
	log.Status, log.Summary, log.MessageCount, log.ErrorMessage, log.ModelID = arg.Status, arg.Summary, arg.MessageCount, arg.ErrorMessage, arg.ModelID
	f.putLog(log)
	return log, nil
}

func (f *fakeQueries) ListMessageAssetsBatch(_ context.Context, _ []pgtype.UUID) ([]sqlc.ListMessageAssetsBatchRow, error) {
	f.queryCalls = append(f.queryCalls, "assets")
	return nil, f.assetsErr
}

func (f *fakeQueries) MeasureHistoryCheckpointCandidates(_ context.Context, arg sqlc.MeasureHistoryCheckpointCandidatesParams) (sqlc.MeasureHistoryCheckpointCandidatesRow, error) {
	var measure sqlc.MeasureHistoryCheckpointCandidatesRow
	for _, row := range f.candidates(arg.AfterTurnPosition, arg.AfterTurnMessageSeq, arg.KeptIds, arg.BeforeTurnPosition, arg.BeforeTurnMessageSeq, arg.MessageIds, arg.ExternalMessageIds) {
		bytes := payloadBytesForTest(row)
		measure.CandidateCount++
		measure.CandidateBytes += bytes
		measure.LargestCandidateBytes = max(measure.LargestCandidateBytes, bytes)
	}
	// A read the test holds open counts as a candidate, so the pass reaches it.
	if measure.CandidateCount == 0 && f.listStarted != nil {
		measure.CandidateCount = 1
	}
	return measure, nil
}

func (f *fakeQueries) ListHistoryCheckpointCandidatesWithinBytes(_ context.Context, arg sqlc.ListHistoryCheckpointCandidatesWithinBytesParams) ([]candidateRow, error) {
	if f.listPanic {
		panic("boom: injected query panic")
	}
	if f.listStarted != nil {
		close(f.listStarted)
	}
	if f.listRelease != nil {
		<-f.listRelease
	}
	candidates := f.candidates(arg.AfterTurnPosition, arg.AfterTurnMessageSeq, arg.KeptIds, arg.BeforeTurnPosition, arg.BeforeTurnMessageSeq, arg.MessageIds, arg.ExternalMessageIds)
	var total int64
	for _, row := range candidates {
		total += payloadBytesForTest(row)
	}
	admitted := make([]candidateRow, 0, len(candidates))
	var cumulative int64
	for _, row := range candidates {
		if cumulative += payloadBytesForTest(row); cumulative > arg.MaxBytes {
			break
		}
		row.CandidateCount, row.CandidateBytes, row.CumulativeBytes = int64(len(candidates)), total, cumulative
		admitted = append(admitted, row)
	}
	return admitted, nil
}

// candidates are the rows a pass reads: the ones the checkpoint it folds
// onto keeps and the ones after its position, before the cutoff and in the
// view when there is one.
func (f *fakeQueries) candidates(afterTurn, afterSeq int64, keptIDs []pgtype.UUID, beforeTurn, beforeSeq pgtype.Int8, viewIDs []pgtype.UUID, viewExternalIDs []string) []candidateRow {
	after := messagepkg.HistoryPosition{Turn: afterTurn, Seq: afterSeq}
	var rows []candidateRow
	for _, row := range f.ordered() {
		position := rowPosition(row)
		if !after.Before(position) && !slices.Contains(keptIDs, row.ID) {
			continue
		}
		if beforeTurn.Valid && !position.Before(messagepkg.HistoryPosition{Turn: beforeTurn.Int64, Seq: beforeSeq.Int64}) {
			continue
		}
		if viewIDs != nil && !slices.Contains(viewIDs, row.ID) && (!row.ExternalMessageID.Valid || !slices.Contains(viewExternalIDs, row.ExternalMessageID.String)) {
			continue
		}
		rows = append(rows, row)
	}
	return rows
}

// payloadBytesForTest measures a row the way the candidate reads do.
func payloadBytesForTest(row candidateRow) int64 {
	return int64(len(row.Content) + len(row.Metadata) + len(row.Usage) + len(row.DisplayText.String))
}

// ordered is the session's context rows in history order, every one placed.
func (f *fakeQueries) ordered() []candidateRow {
	var last int64
	for _, row := range f.history {
		if row.TurnPosition.Valid {
			last = max(last, row.TurnPosition.Int64)
		}
	}
	for i := range f.history {
		if !f.history[i].TurnPosition.Valid {
			last++
			f.history[i].TurnPosition = pgtype.Int8{Int64: last, Valid: true}
			f.history[i].TurnMessageSeq = pgtype.Int8{Int64: 1, Valid: true}
		}
	}
	rows := slices.Clone(f.history)
	slices.SortStableFunc(rows, func(a, b candidateRow) int {
		return cmp.Or(cmp.Compare(a.TurnPosition.Int64, b.TurnPosition.Int64), cmp.Compare(a.TurnMessageSeq.Int64, b.TurnMessageSeq.Int64))
	})
	return rows
}

func (f *fakeQueries) GetActiveHistoryCheckpoint(ctx context.Context, _ pgtype.UUID) (sqlc.BotHistoryCheckpoint, error) {
	return f.GetHistoryCheckpoint(ctx, f.active)
}

func (f *fakeQueries) GetHistoryCheckpoint(_ context.Context, id pgtype.UUID) (sqlc.BotHistoryCheckpoint, error) {
	row, ok := f.checkpoints[id]
	if !ok {
		return sqlc.BotHistoryCheckpoint{}, pgx.ErrNoRows
	}
	return row, nil
}

func (f *fakeQueries) ListHistoryCheckpointChain(_ context.Context, id pgtype.UUID) ([]sqlc.BotHistoryCheckpoint, error) {
	var chain []sqlc.BotHistoryCheckpoint
	for row, ok := f.checkpoints[id]; ok; row, ok = f.checkpoints[row.ParentID] {
		chain = append(chain, row)
	}
	return chain, nil
}

func (f *fakeQueries) CheckHistoryCheckpoint(_ context.Context, id pgtype.UUID) (sqlc.CheckHistoryCheckpointRow, error) {
	row, ok := f.checkpoints[id]
	if !ok {
		return sqlc.CheckHistoryCheckpointRow{}, pgx.ErrNoRows
	}
	history := f.ordered()
	var result sqlc.CheckHistoryCheckpointRow
	for _, item := range decodeItemsForTest(row.Items) {
		if item.RowID != "" && !slices.ContainsFunc(history, func(h candidateRow) bool { return formatUUID(h.ID) == item.RowID }) {
			result.MissingRows++
		}
		if log, ok := f.logs[db.ParseUUIDOrEmpty(item.SummaryID)]; item.SummaryID != "" && (!ok || log.Status != "ok" || strings.TrimSpace(log.Summary) == "") {
			result.MissingSummaries++
		}
	}
	return result, nil
}

func (f *fakeQueries) ListHistoryCheckpointSummaries(_ context.Context, arg sqlc.ListHistoryCheckpointSummariesParams) ([]sqlc.ListHistoryCheckpointSummariesRow, error) {
	var rows []sqlc.ListHistoryCheckpointSummariesRow
	for _, id := range arg.Ids {
		if log, ok := f.logs[id]; ok && log.Status == "ok" {
			rows = append(rows, sqlc.ListHistoryCheckpointSummariesRow{ID: id, Summary: log.Summary})
		}
	}
	return rows, nil
}

func (f *fakeQueries) LockHistoryCheckpointSession(_ context.Context, _ sqlc.LockHistoryCheckpointSessionParams) (pgtype.UUID, error) {
	f.queryCalls = append(f.queryCalls, "lock")
	return f.active, nil
}

func (f *fakeQueries) ListHistoryCheckpointSpanRows(_ context.Context, arg sqlc.ListHistoryCheckpointSpanRowsParams) ([]sqlc.ListHistoryCheckpointSpanRowsRow, error) {
	after := messagepkg.HistoryPosition{Turn: arg.AfterTurnPosition, Seq: arg.AfterTurnMessageSeq}
	through := messagepkg.HistoryPosition{Turn: arg.ThroughTurnPosition, Seq: arg.ThroughTurnMessageSeq}
	var rows []sqlc.ListHistoryCheckpointSpanRowsRow
	for _, row := range f.ordered() {
		if position := rowPosition(row); after.Before(position) && !through.Before(position) || slices.Contains(arg.Ids, row.ID) {
			rows = append(rows, sqlc.ListHistoryCheckpointSpanRowsRow{ID: row.ID, TurnPosition: row.TurnPosition, TurnMessageSeq: row.TurnMessageSeq})
		}
	}
	return rows, nil
}

func (f *fakeQueries) CreateHistoryCheckpoint(_ context.Context, arg sqlc.CreateHistoryCheckpointParams) (sqlc.BotHistoryCheckpoint, error) {
	row := sqlc.BotHistoryCheckpoint{
		ID:             pgtype.UUID{Bytes: uuid.New(), Valid: true},
		SessionID:      arg.SessionID,
		ParentID:       arg.ParentID,
		TurnPosition:   arg.TurnPosition,
		TurnMessageSeq: arg.TurnMessageSeq,
		Items:          arg.Items,
		CreatedAt:      pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	f.putCheckpoint(row)
	// What the checkpoint folds: the rows after its parent's position through
	// its own, and the rows its parent keeps, that it does not keep itself.
	parent := f.checkpoints[row.ParentID]
	after := messagepkg.HistoryPosition{Turn: parent.TurnPosition, Seq: parent.TurnMessageSeq}
	through := messagepkg.HistoryPosition{Turn: row.TurnPosition, Seq: row.TurnMessageSeq}
	parentItems, items := decodeItemsForTest(parent.Items), decodeItemsForTest(row.Items)
	f.folded = nil
	for _, h := range f.ordered() {
		id, position := formatUUID(h.ID), rowPosition(h)
		inSpan := after.Before(position) && !through.Before(position) || slices.Contains(parentItems, checkpoint.Item{RowID: id})
		if inSpan && !slices.Contains(items, checkpoint.Item{RowID: id}) {
			f.folded = append(f.folded, h.ID)
		}
	}
	return row, nil
}

func (f *fakeQueries) SetHistoryCheckpoint(_ context.Context, arg sqlc.SetHistoryCheckpointParams) (int64, error) {
	if f.active != arg.ExpectedCheckpointID {
		return 0, nil
	}
	f.active = arg.CheckpointID
	return 1, nil
}

func (f *fakeQueries) putLog(log sqlc.BotHistoryMessageCompact) {
	if f.logs == nil {
		f.logs = make(map[pgtype.UUID]sqlc.BotHistoryMessageCompact)
	}
	f.logs[log.ID] = log
}

func (f *fakeQueries) putCheckpoint(row sqlc.BotHistoryCheckpoint) {
	if f.checkpoints == nil {
		f.checkpoints = make(map[pgtype.UUID]sqlc.BotHistoryCheckpoint)
	}
	f.checkpoints[row.ID] = row
}

// hold makes a checkpoint after the active one that holds the summaries, in
// order, at the start of the history the session lists, and makes it the
// session's active one.
func (f *fakeQueries) hold(summaries ...string) sqlc.BotHistoryCheckpoint {
	items := make([]checkpoint.Item, 0, len(summaries))
	for _, summary := range summaries {
		log := sqlc.BotHistoryMessageCompact{ID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, Status: "ok", Summary: summary}
		f.putLog(log)
		items = append(items, checkpoint.Item{SummaryID: formatUUID(log.ID)})
	}
	encoded, _ := json.Marshal(items)
	row := sqlc.BotHistoryCheckpoint{
		ID:        pgtype.UUID{Bytes: uuid.New(), Valid: true},
		ParentID:  f.active,
		Items:     encoded,
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	f.putCheckpoint(row)
	f.active = row.ID
	return row
}

// held is what the session's active checkpoint holds.
func (f *fakeQueries) held() []checkpoint.Item {
	return decodeItemsForTest(f.checkpoints[f.active].Items)
}

func decodeItemsForTest(raw []byte) []checkpoint.Item {
	var items []checkpoint.Item
	_ = json.Unmarshal(raw, &items)
	return items
}

// summaryIDs names the summaries a checkpoint holds, in order.
func summaryIDs(row sqlc.BotHistoryCheckpoint) []string {
	var ids []string
	for _, item := range decodeItemsForTest(row.Items) {
		if item.SummaryID != "" {
			ids = append(ids, item.SummaryID)
		}
	}
	return ids
}

// --- harness ------------------------------------------------------------------

func newMachineryService(q dbstore.Queries) *Service {
	return NewService(slog.New(slog.DiscardHandler), q)
}

const (
	passNoop     = "noop"
	passOK       = "ok"
	passProgress = "progress"
)

// passResult is the outcome of compactOnce: whether a pass committed and
// left candidates behind, its summary and the rows it folded.
type passResult struct {
	Status       string
	Summary      string
	MessageCount int
}

// runPass runs one pass of an event on its own.
func (s *Service) runPass(ctx context.Context, cfg TriggerConfig) (Pass, bool, error) {
	return s.doCompaction(ctx, db.ParseUUIDOrEmpty(cfg.BotID), db.ParseUUIDOrEmpty(cfg.SessionID), cfg, "")
}

// compactOnce runs one pass of an event on its own and reports its outcome.
func (s *Service) compactOnce(ctx context.Context, cfg TriggerConfig) (passResult, error) {
	pass, folded, err := s.runPass(ctx, cfg)
	if err != nil || !folded {
		return passResult{Status: passNoop}, err
	}
	result := passResult{Status: passOK, Summary: pass.Summary}
	if pass.More {
		result.Status = passProgress
	}
	for _, source := range pass.Covered {
		if source.Kind == "history" {
			result.MessageCount++
		}
	}
	return result, nil
}

func machineryConfig(stub *stubModel, historyBudgetTokens int) TriggerConfig {
	return TriggerConfig{
		BotID:               uuid.NewString(),
		SessionID:           uuid.NewString(),
		ModelID:             "stub-model",
		ClientType:          "openai-completions",
		APIKey:              "test",
		BaseURL:             "http://stub.invalid",
		HTTPClient:          &http.Client{Transport: stub},
		SummaryWindowTokens: 128000,
		HistoryBudgetTokens: historyBudgetTokens,
	}
}

// currentConfig is machineryConfig for a request whose current input is
// current.
func currentConfig(stub *stubModel, historyBudgetTokens int, current candidateRow) TriggerConfig {
	cfg := machineryConfig(stub, historyBudgetTokens)
	cfg.ProtectedSources = historySources(current)
	return cfg
}

func idSet(ids []pgtype.UUID) map[pgtype.UUID]bool {
	m := make(map[pgtype.UUID]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// machineryCorpus returns a deterministic session whose oldest portion contains
// two tool exchanges (one base64-image result, one structured stdout result),
// plus recent text turns. Indices are returned for precise assertions.
func machineryCorpus(t *testing.T) []candidateRow {
	t.Helper()
	b64 := strings.Repeat("QUJD", 100) // 400 base64 chars
	return []candidateRow{
		mkRow(t, "user", `[{"type":"text","text":"deploy please"}]`, 100),                                                                                          // 0
		mkRow(t, "assistant", `[{"type":"text","text":"on it"},{"type":"tool-call","toolCallId":"A","toolName":"screenshot","input":{}}]`, 100),                    // 1 call A
		mkRow(t, "tool", `[{"type":"tool-result","toolCallId":"A","toolName":"screenshot","result":{"mime":"image/png","data":"`+b64+`"}}]`, 100),                  // 2 result A (base64)
		mkRow(t, "assistant", `[{"type":"text","text":"captured the screen"}]`, 100),                                                                               // 3
		mkRow(t, "user", `[{"type":"text","text":"now build"}]`, 100),                                                                                              // 4
		mkRow(t, "assistant", `[{"type":"text","text":"running"},{"type":"tool-call","toolCallId":"B","toolName":"exec_command","input":{"cmd":"make"}}]`, 100),    // 5 call B
		mkRow(t, "tool", `[{"type":"tool-result","toolCallId":"B","toolName":"exec_command","result":{"exit_code":0,"stdout":"build ok done","stderr":""}}]`, 100), // 6 result B (structured)
		mkRow(t, "assistant", `[{"type":"text","text":"build finished"}]`, 100),                                                                                    // 7
		mkRow(t, "user", `[{"type":"text","text":"recent question"}]`, 100),                                                                                        // 8
		mkRow(t, "assistant", `[{"type":"text","text":"recent answer"}]`, 100),                                                                                     // 9
	}
}

// --- tests --------------------------------------------------------------------

// A pass that folds only what precedes an input the request keeps shows that
// input after the summary, where the request keeps it, not as kept in place.
func TestDoCompactionShowsAKeptInputAfterTheSummaryItFollows(t *testing.T) {
	rows := []candidateRow{
		mkRow(t, "user", `"old question"`, 100),
		mkRow(t, "assistant", `"old answer"`, 100),
		mkRow(t, "user", `"KEPT_INPUT"`, 100),
		mkRow(t, "assistant", `"LATEST_STEP"`, 100),
	}
	q := &fakeQueries{history: rows}
	stub := &stubModel{summary: "SUMMARY-OK"}
	cfg := currentConfig(stub, 450, rows[3])
	cfg.Anchors = historySources(rows[2])
	if _, err := newMachineryService(q).compactOnce(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	folded := idSet(q.folded)
	if len(folded) != 2 || !folded[rows[0].ID] || !folded[rows[1].ID] {
		t.Fatalf("folded %d rows, want the two before the input", len(folded))
	}
	_, rendered, _ := strings.Cut(stub.prompt, "<conversation>")
	conversation, current, _ := strings.Cut(rendered, "<current_request>")
	if strings.Contains(conversation, "KEPT_INPUT") || !strings.Contains(current, "user: KEPT_INPUT\nassistant: LATEST_STEP") {
		t.Fatalf("the kept input must read after the summary, before the latest step:\n%s", stub.prompt)
	}
}

func TestDoCompactionFoldsToolAwareWindowAndRendersCleanPrompt(t *testing.T) {
	rows := machineryCorpus(t)
	q := &fakeQueries{history: rows}
	stub := &stubModel{summary: "SUMMARY-OK"}
	svc := newMachineryService(q)

	// Everything before the current input (row 7) is folded, with both tool
	// exchanges kept whole on the folded side.
	cfg := machineryConfig(stub, 450)
	cfg.ProtectedSources = historySources(rows[7])
	if _, err := svc.compactOnce(context.Background(), cfg); err != nil {
		t.Fatalf("compact: %v", err)
	}

	if stub.calls != 1 {
		t.Fatalf("summarizer called %d times, want 1", stub.calls)
	}
	if len(q.folded) != 7 {
		t.Fatalf("folded %d messages, want 7 (0..6, tool exchange kept whole)", len(q.folded))
	}
	folded := idSet(q.folded)
	for i := 0; i <= 6; i++ {
		if !folded[rows[i].ID] {
			t.Fatalf("row %d should be folded", i)
		}
	}
	for i := 7; i <= 9; i++ {
		if folded[rows[i].ID] {
			t.Fatalf("row %d (recent) should NOT be folded", i)
		}
	}
	if !folded[rows[6].ID] {
		t.Fatalf("tool result B must be pulled into the fold with its call")
	}
	if q.completed.Status != "ok" || q.completed.Summary != "SUMMARY-OK" || q.completed.MessageCount != 7 {
		t.Fatalf("complete log = status=%q summary=%q count=%d", q.completed.Status, q.completed.Summary, q.completed.MessageCount)
	}
	// The rows after the last one it folds stay after the checkpoint, which
	// holds only the summary.
	if held := q.held(); len(held) != 1 || held[0].SummaryID != formatUUID(q.createdLogID) {
		t.Fatalf("checkpoint holds %+v, want only the summary", held)
	}
	if position := q.checkpoints[q.active]; position.TurnPosition != rows[6].TurnPosition.Int64 || position.TurnMessageSeq != rows[6].TurnMessageSeq.Int64 {
		t.Fatalf("checkpoint at (%d, %d), want the last folded row's position", position.TurnPosition, position.TurnMessageSeq)
	}

	// The summarizer prompt must carry clean rendered outcomes, no media, no noise.
	if !strings.Contains(stub.prompt, "build ok done") {
		t.Fatalf("structured tool outcome missing from prompt:\n%s", stub.prompt)
	}
	if !strings.Contains(stub.prompt, `"cmd":"make"`) {
		t.Fatalf("tool call arguments missing from prompt:\n%s", stub.prompt)
	}
	if !strings.Contains(stub.prompt, "[media]") || strings.Contains(stub.prompt, "QUJDQUJDQUJD") {
		t.Fatalf("base64 image result not scrubbed in prompt")
	}
	if strings.Contains(stub.prompt, `{"type":"text"`) || strings.Contains(stub.prompt, `"toolCallId"`) {
		t.Fatalf("raw JSON-envelope noise leaked into prompt:\n%s", stub.prompt)
	}
}

// TestDoCompactionFailsClosedWhenEntryFloorsExceedBudget drives one
// unsplittable tool exchange larger than the summarizer can read: sending it
// anyway would overflow the summarizer window, so the attempt must fail
// closed with no log and zero provider calls.
func TestDoCompactionFailsClosedWhenEntryFloorsExceedBudget(t *testing.T) {
	const fanout = 40
	callParts := make([]string, 0, fanout)
	for i := 0; i < fanout; i++ {
		callParts = append(callParts, fmt.Sprintf(`{"type":"tool-call","toolCallId":"c%d","toolName":"probe","input":{}}`, i))
	}
	rows := []candidateRow{
		mkRow(t, "assistant", "["+strings.Join(callParts, ",")+"]", 100),
	}
	for i := 0; i < fanout; i++ {
		rows = append(rows, mkRow(t, "tool", fmt.Sprintf(`[{"type":"tool-result","toolCallId":"c%d","toolName":"probe","output":{"type":"text","value":"ok"}}]`, i), 100))
	}
	rows = append(rows, mkRow(t, "user", `[{"type":"text","text":"recent question"}]`, 100))

	q := &fakeQueries{history: rows}
	stub := &stubModel{summary: "SUMMARY-OK"}
	svc := newMachineryService(q)

	cfg := machineryConfig(stub, 100)
	cfg.SummaryWindowTokens = 1000

	_, err := svc.compactOnce(context.Background(), cfg)
	if !errors.Is(err, errCompactionInputOverflow) {
		t.Fatalf("compact error = %v, want errCompactionInputOverflow", err)
	}
	if stub.calls != 0 {
		t.Fatalf("summarizer calls = %d, want 0: an oversized selection must not reach the provider", stub.calls)
	}
	if q.created {
		t.Fatal("a log was opened: fail closed before the pass starts")
	}
	if q.active.Valid {
		t.Fatalf("checkpoint = %v, want none", q.active)
	}
}

func TestDoCompactionAllEmptyWindowSkipsModelAndFolding(t *testing.T) {
	rows := []candidateRow{
		mkRow(t, "assistant", `[{"type":"reasoning","text":"thinking a"}]`, 100),
		mkRow(t, "assistant", `[{"type":"reasoning","text":"thinking b"}]`, 100),
		mkRow(t, "assistant", `[{"type":"text","text":"recent kept"}]`, 100),
	}
	q := &fakeQueries{history: rows}
	stub := &stubModel{summary: "unused"}
	svc := newMachineryService(q)

	cfg := machineryConfig(stub, 150)
	cfg.ProtectedSources = historySources(rows[2])
	if _, err := svc.compactOnce(context.Background(), cfg); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if stub.calls != 0 {
		t.Fatalf("summarizer should not be called for an all-empty window (calls=%d)", stub.calls)
	}
	if q.active.Valid {
		t.Fatalf("nothing should be folded for an all-empty window (checkpoint=%v)", q.active)
	}
	if q.created {
		t.Fatal("a no-op compaction must not create a log row")
	}
}

func TestDoCompactionCoversToolExchangeWhoseResultRendersEmpty(t *testing.T) {
	rows := []candidateRow{
		toolCallRow(t, 100),
		mkRow(t, "tool", `[]`, 100),
		mkRow(t, "assistant", `[{"type":"text","text":"recent kept"}]`, 100),
	}
	q := &fakeQueries{history: rows}
	stub := &stubModel{summary: "unused"}
	svc := newMachineryService(q)

	cfg := machineryConfig(stub, 150)
	cfg.ProtectedSources = historySources(rows[2])
	if _, err := svc.compactOnce(context.Background(), cfg); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if stub.calls != 1 || len(q.folded) != 2 || q.folded[0] != rows[0].ID || q.folded[1] != rows[1].ID {
		t.Fatalf("calls=%d folded=%d, want the call and its empty result covered together", stub.calls, len(q.folded))
	}
}

func TestDoCompactionCoversEmptyMiddleRowWithItsNeighbours(t *testing.T) {
	// Window: an empty-rendering reasoning row sits between two rendered rows.
	// All three fold together, so the summary stands for one contiguous run
	// and the checkpoint keeps none of them.
	rows := []candidateRow{
		mkRow(t, "user", `[{"type":"text","text":"old question about a long-running project with plenty of detail to summarize"}]`, 100), // 0
		mkRow(t, "assistant", `[{"type":"reasoning","text":"thinking"}]`, 100),                                                           // 1 renders empty
		mkRow(t, "assistant", `[{"type":"text","text":"old answer covering the whole project state in enough words to compress"}]`, 100), // 2
		mkRow(t, "user", `[{"type":"text","text":"recent question"}]`, 100),                                                              // 3 kept
		mkRow(t, "assistant", `[{"type":"text","text":"recent answer"}]`, 100),                                                           // 4 kept
	}
	q := &fakeQueries{history: rows}
	stub := &stubModel{summary: "SUMMARY"}
	svc := newMachineryService(q)

	cfg := machineryConfig(stub, 250)
	cfg.ProtectedSources = historySources(rows[3])
	if _, err := svc.compactOnce(context.Background(), cfg); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if len(q.folded) != 3 || q.folded[0] != rows[0].ID || q.folded[1] != rows[1].ID || q.folded[2] != rows[2].ID {
		t.Fatalf("folded = %d ids, want rows 0-2 as one contiguous run", len(q.folded))
	}
	if q.completed.Status != "ok" || q.completed.MessageCount != 3 {
		t.Fatalf("complete log = status=%q count=%d, want ok/3", q.completed.Status, q.completed.MessageCount)
	}
}

func TestDoCompactionEmptyHistoryNoOp(t *testing.T) {
	q := &fakeQueries{}
	stub := &stubModel{summary: "unused"}
	svc := newMachineryService(q)

	if _, err := svc.compactOnce(context.Background(), machineryConfig(stub, 100)); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if stub.calls != 0 || q.active.Valid {
		t.Fatalf("empty history must be a no-op (calls=%d checkpoint=%v)", stub.calls, q.active)
	}
	if q.created {
		t.Fatal("empty history must not create a log row")
	}
}

// failingModel refuses every request with an answer the summarizer does not
// retry, so each compaction attempt makes exactly one call.
type failingModel struct{ calls int }

func (f *failingModel) RoundTrip(*http.Request) (*http.Response, error) {
	f.calls++
	return &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"boom"}}`)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// rateLimitedOnce answers the first request with a rate limit and passes the
// rest to the stub.
type rateLimitedOnce struct {
	stub  *stubModel
	calls int
}

func (r *rateLimitedOnce) RoundTrip(req *http.Request) (*http.Response, error) {
	r.calls++
	if r.calls == 1 {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"slow down","type":"rate_limit_error"}}`)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	}
	return r.stub.RoundTrip(req)
}

// A summarizer call the provider rate-limits is made again, and the pass
// completes with the retried call's summary.
func TestDoCompactionRetriesRateLimitedSummarizerCall(t *testing.T) {
	t.Parallel()

	q := &fakeQueries{history: machineryCorpus(t)}
	stub := &stubModel{summary: "SUMMARY-RETRIED"}
	transport := &rateLimitedOnce{stub: stub}
	cfg := machineryConfig(stub, 450)
	cfg.HTTPClient = &http.Client{Transport: transport}

	res, err := newMachineryService(q).compactOnce(context.Background(), cfg)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if transport.calls != 2 || res.Status != passOK || res.Summary != "SUMMARY-RETRIED" {
		t.Fatalf("result = %+v after %d calls, want the retried summary after 2", res, transport.calls)
	}
}

// A pass whose summarizer fails settles its log as an error and commits
// nothing: the session keeps its checkpoint and the rows stay candidates.
func TestDoCompactionSummarizerFailureRecordsErrorAndKeepsTheCheckpoint(t *testing.T) {
	for name, transport := range map[string]http.RoundTripper{
		"failed call":   &failingModel{},
		"empty summary": &stubModel{summary: "   "},
	} {
		t.Run(name, func(t *testing.T) {
			q := &fakeQueries{history: machineryCorpus(t)}
			parent := q.hold("earlier summary")
			cfg := machineryConfig(&stubModel{}, 450)
			cfg.HTTPClient = &http.Client{Transport: transport}

			if _, err := newMachineryService(q).compactOnce(context.Background(), cfg); err == nil {
				t.Fatal("a failed summary must surface an error")
			}
			if !q.created || q.completed.Status != "error" || q.logs[q.createdLogID].Status != "error" {
				t.Fatalf("a failed attempt must leave an error log row (created=%v status=%q)", q.created, q.completed.Status)
			}
			if q.active != parent.ID || len(q.folded) != 0 {
				t.Fatalf("checkpoint = %v folded=%d, want %v kept with nothing folded", q.active, len(q.folded), parent.ID)
			}
		})
	}
}
