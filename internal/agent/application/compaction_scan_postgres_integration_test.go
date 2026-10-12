package application

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

func (f progressFixture) uuid(id string) pgtype.UUID {
	f.t.Helper()
	var value pgtype.UUID
	if err := value.Scan(id); err != nil {
		f.t.Fatal(err)
	}
	return value
}

type scanReadMeter struct {
	dbstore.Queries
	windows        int
	bytes          int64
	afterFirstRead func()
}

type scanPassRunner func(context.Context, compaction.TriggerConfig) (compaction.Result, error)

func (run scanPassRunner) RunCompactionSync(ctx context.Context, cfg compaction.TriggerConfig) (compaction.Result, error) {
	return run(ctx, cfg)
}

func (m *scanReadMeter) ListUncompactedMessagesBySessionWithinBytes(ctx context.Context, p dbsqlc.ListUncompactedMessagesBySessionWithinBytesParams) ([]dbsqlc.ListUncompactedMessagesBySessionWithinBytesRow, error) {
	rows, err := m.Queries.ListUncompactedMessagesBySessionWithinBytes(ctx, p)
	m.windows++
	if len(rows) > 0 && !rows[0].Oversized {
		m.bytes += rows[len(rows)-1].CumulativeBytes
	}
	if m.windows == 1 && m.afterFirstRead != nil {
		m.afterFirstRead()
	}
	return rows, err
}

func TestPostgresCompactionAbandonsScanAfterEpochChange(t *testing.T) {
	f, store := newProgressFixture(t)
	for n := 0; n < 60; n++ {
		f.persist("assistant", []map[string]any{{"type": "reasoning", "text": strings.Repeat("r", 16<<10)}})
	}
	later := f.text("assistant", strings.Repeat("later history. ", 300))
	f.text("user", "current question")
	model := &countingSummarizer{summary: "condensed"}
	meter := &scanReadMeter{Queries: store, afterFirstRead: func() {
		if _, err := f.pool.Exec(f.ctx, "UPDATE bot_sessions SET compaction_epoch=compaction_epoch+1 WHERE id=$1", f.sessionID); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := compaction.NewService(slog.New(slog.DiscardHandler), meter).RunCompactionSync(f.ctx, f.config(model)); err == nil || model.calls != 0 {
		t.Fatalf("mixed-epoch scan reached provider: err=%v calls=%d", err, model.calls)
	}
	state, err := f.queries.GetCompactionScan(f.ctx, f.uuid(f.sessionID))
	if err != nil || state.CompactionScanAfter.Valid {
		t.Fatalf("old scan survived epoch change: %+v, %v", state, err)
	}
	if _, err := compaction.NewService(slog.New(slog.DiscardHandler), store).RunCompactionSync(f.ctx, f.config(model)); err != nil {
		t.Fatal(err)
	}
	if _, status := f.claim(later.ID); status != "ok" {
		t.Fatal("fresh epoch did not resume progress")
	}
}

func TestPostgresCompactionInputOverflowDoesNotCoolDownLaterHistory(t *testing.T) {
	f, store := newProgressFixture(t)
	var calls []map[string]any
	for n := 0; n < 40; n++ {
		calls = append(calls, map[string]any{"type": "tool-call", "toolCallId": strconv.Itoa(n), "toolName": "exec", "input": map[string]any{"command": "true"}})
	}
	f.persist("assistant", calls)
	for n := 0; n < 40; n++ {
		f.persist("tool", []map[string]any{{"type": "tool-result", "toolCallId": strconv.Itoa(n), "toolName": "exec", "output": map[string]any{"type": "text", "value": "done"}}})
	}
	later := f.text("assistant", strings.Repeat("later history. ", 100))
	f.text("user", "current question")
	model := &countingSummarizer{summary: "condensed"}
	cfg := f.config(model)
	cfg.MaxCompactTokens = 100
	svc := compaction.NewService(slog.New(slog.DiscardHandler), store)
	if _, err := svc.RunCompactionSync(f.ctx, cfg); err == nil || model.calls != 0 {
		t.Fatalf("unsplittable group exceeded the provider budget: err=%v calls=%d", err, model.calls)
	}
	res, err := svc.RunCompactionSync(f.ctx, cfg)
	if _, status := f.claim(later.ID); err != nil || status != "ok" || model.calls != 1 {
		t.Fatalf("local input overflow blocked later history: %+v, %v status=%q calls=%d", res, err, status, model.calls)
	}
}

func TestPostgresCompactionResumesBoundedScanAcrossRestart(t *testing.T) {
	f, store := newProgressFixture(t)
	for n := 0; n < 2300; n++ {
		f.persist("assistant", []map[string]any{{"type": "reasoning", "text": strings.Repeat("r", 16<<10)}})
	}
	later := f.text("assistant", strings.Repeat("later history. ", 400))
	f.text("user", "current question")
	model := &countingSummarizer{summary: "later history condensed"}
	cfg := f.config(model)
	cfg.MaxCompactTokens = 100000
	passes := 0
	runner := scanPassRunner(func(ctx context.Context, cfg compaction.TriggerConfig) (compaction.Result, error) {
		meter := &scanReadMeter{Queries: store}
		res, err := compaction.NewService(slog.New(slog.DiscardHandler), meter).RunCompactionSync(ctx, cfg)
		if err != nil || meter.bytes > 32<<20 || meter.windows > 64 {
			t.Fatalf("pass=%d result=%+v err=%v bytes=%d windows=%d", passes, res, err, meter.bytes, meter.windows)
		}
		t.Logf("pass=%d status=%s bytes=%d windows=%d calls=%d", passes, res.Status, meter.bytes, meter.windows, model.calls)
		if passes == 0 && (model.calls != 0 || res.Summary != "" || res.MessageCount != 0 || res.Status != compaction.StatusNoop) {
			t.Fatal("fixture must require more than one bounded scan")
		}
		passes++
		return res, err
	})
	svc := &Service{logger: slog.New(slog.DiscardHandler), compactionService: runner}
	if err := svc.drainCompactionBacklog(f.ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if _, status := f.claim(later.ID); status != "ok" || model.calls != 1 || passes > maxAsyncCompactionPasses {
		t.Fatalf("automatic drain stranded the suffix: status=%q calls=%d passes=%d", status, model.calls, passes)
	}
}

func TestPostgresCompactionRevisitsAfterBudgetChangeDuringGrowth(t *testing.T) {
	f, store := newProgressFixture(t)
	a := f.text("assistant", strings.Repeat("original work. ", 200))
	f.reasoning()
	b := f.text("assistant", strings.Repeat("later work. ", 200))
	f.text("user", "current question")
	model := &countingSummarizer{summary: "condensed"}
	cfg := f.config(model)
	cfg.MaxCompactTokens = 1
	svc := compaction.NewService(slog.New(slog.DiscardHandler), store)
	if _, err := svc.RunCompactionSync(f.ctx, cfg); err == nil || model.calls != 0 {
		t.Fatalf("impossible prompt budget reached provider: err=%v calls=%d", err, model.calls)
	}
	initial, err := f.queries.GetCompactionScan(f.ctx, f.uuid(f.sessionID))
	if err != nil || initial.CompactionScanAfter != f.uuid(a.ID) {
		t.Fatalf("scan must stop at the rejected group: %+v, %v", initial, err)
	}
	cfg.MaxCompactTokens, cfg.Manual = 30000, true
	for pass := 0; pass < 4; pass++ {
		f.text("user", fmt.Sprintf("new question %d", pass))
		f.text("assistant", strings.Repeat("new history. ", 100))
		if res, err := svc.RunCompactionSync(f.ctx, cfg); err != nil {
			t.Fatalf("pass=%d result=%+v err=%v", pass, res, err)
		}
		if pass == 0 {
			if _, status := f.claim(b.ID); status != "ok" {
				t.Fatal("budget change reset the cursor before reaching the later span")
			}
		}
		if _, status := f.claim(a.ID); status == "ok" {
			return
		}
	}
	t.Fatal("continuous appends prevented the old rejected group from being reconsidered")
}

func TestPostgresCompactionWaitsForCompleteToolResults(t *testing.T) {
	for _, arrivesAfterRead := range []bool{false, true} {
		t.Run(fmt.Sprintf("arrives_after_read=%t", arrivesAfterRead), func(t *testing.T) {
			f, store := newProgressFixture(t)
			task := f.text("user", "start task")
			call := f.persist("assistant", []map[string]any{
				{"type": "tool-call", "toolCallId": "c1", "toolName": "exec", "input": map[string]any{"command": "first"}},
				{"type": "tool-call", "toolCallId": "c2", "toolName": "exec", "input": map[string]any{"command": "second"}},
			})
			first := f.persist("tool", []map[string]any{{"type": "tool-result", "toolCallId": "c1", "toolName": "exec", "output": map[string]any{"type": "text", "value": strings.Repeat("first result. ", 100)}}})
			f.text("user", "next task")
			var cutoff pgtype.Timestamptz
			if err := f.pool.QueryRow(f.ctx, "UPDATE bot_sessions SET compaction_scan_at=clock_timestamp(), compaction_scan_epoch=compaction_epoch WHERE id=$1 RETURNING compaction_scan_at", f.sessionID).Scan(&cutoff); err != nil {
				t.Fatal(err)
			}
			var second messagepkg.Message
			addResult := func() {
				raw := []byte(`{"role":"tool","content":[{"type":"tool-result","toolCallId":"c2","toolName":"exec","output":{"type":"text","value":"done"}}]}`)
				var err error
				second, err = f.messages.Persist(f.ctx, messagepkg.PersistInput{BotID: f.botID, SessionID: f.sessionID, Role: "tool", Content: raw, TurnRequestMessageID: task.ID})
				if err != nil {
					t.Fatal(err)
				}
			}
			if !arrivesAfterRead {
				addResult()
			}
			model := &countingSummarizer{summary: "condensed tool output"}
			cfg := f.config(model)
			svc := compaction.NewService(slog.New(slog.DiscardHandler), store)
			_, _ = svc.RunCompactionSync(f.ctx, cfg)
			for _, row := range []messagepkg.Message{call, first} {
				if claim, _ := f.claim(row.ID); claim != "" {
					t.Fatalf("incomplete exchange row %s was claimed", row.ID)
				}
			}
			if arrivesAfterRead {
				addResult()
			}
			for pass := 0; pass < 4; pass++ {
				res, err := svc.RunCompactionSync(f.ctx, cfg)
				t.Logf("pass=%d result=%+v err=%v calls=%d", pass, res, err, model.calls)
				if _, status := f.claim(call.ID); status == "ok" {
					claim, _ := f.claim(call.ID)
					for _, row := range []messagepkg.Message{first, second} {
						if got, status := f.claim(row.ID); got != claim || status != "ok" {
							t.Fatalf("tool result %s not claimed with its call", row.ID)
						}
					}
					return
				}
			}
			t.Fatal("completed exchange did not become reachable in a later scan round")
		})
	}
}

func TestPostgresCompactionReadsToolExchangeAcrossWindowWhole(t *testing.T) {
	f, store := newProgressFixture(t)
	f.persist("assistant", []map[string]any{{"type": "reasoning", "text": strings.Repeat("r", 400<<10)}})
	call := f.persist("assistant", []map[string]any{{"type": "tool-call", "toolCallId": "large", "toolName": "exec", "input": map[string]any{"command": "true"}}})
	result := f.persist("tool", []map[string]any{{"type": "tool-result", "toolCallId": "large", "toolName": "exec", "output": map[string]any{"type": "text", "value": strings.Repeat("r", 300<<10)}}})
	f.text("user", "current question")
	model := &countingSummarizer{summary: "tool output condensed"}
	meter := &scanReadMeter{Queries: store}
	res, err := compaction.NewService(slog.New(slog.DiscardHandler), meter).RunCompactionSync(f.ctx, f.config(model))
	if err != nil || res.Status != compaction.StatusOK || model.calls != 1 || meter.windows != 2 {
		t.Fatalf("split exchange: %+v, %v calls=%d windows=%d", res, err, model.calls, meter.windows)
	}
	claim, status := f.claim(call.ID)
	if other, otherStatus := f.claim(result.ID); claim != other || status != "ok" || otherStatus != "ok" {
		t.Fatal("window boundary split the tool exchange")
	}
}

func TestPostgresCompactionNeverClaimsAcrossAnExistingSummary(t *testing.T) {
	f, store := newProgressFixture(t)
	left := f.text("assistant", "left fragment")
	pending := f.pending(left.ID)
	middle := f.text("assistant", strings.Repeat("middle history. ", 300))
	f.text("user", "old question")
	model := &countingSummarizer{summary: strings.Repeat("s", 4*254-21)}
	cfg := f.config(model)
	svc := compaction.NewService(slog.New(slog.DiscardHandler), store)
	if res, err := svc.RunCompactionSync(f.ctx, cfg); err != nil || res.Status != compaction.StatusOK {
		t.Fatalf("seed middle summary: %+v, %v", res, err)
	}
	middleClaim, _ := f.claim(middle.ID)
	if _, err := f.pool.Exec(f.ctx, "UPDATE bot_history_message_compacts SET started_at=now()-INTERVAL '16 minutes' WHERE id=$1", pending); err != nil {
		t.Fatal(err)
	}
	right := f.text("assistant", strings.Repeat("right history. ", 300))
	current := f.text("user", "current question")
	for pass := 0; pass < 5; pass++ {
		_, _ = svc.RunCompactionSync(f.ctx, cfg)
		if _, status := f.claim(right.ID); status == "ok" {
			break
		}
	}
	if _, status := f.claim(left.ID); status == "ok" {
		t.Fatal("left fragment joined a claim across the middle summary")
	}
	if claim, status := f.claim(right.ID); status != "ok" || claim == middleClaim {
		t.Fatal("right history was not committed independently")
	}
	if replay := strings.Join(f.replay(map[string]string{left.ID: "left", current.ID: "current"}), ","); replay != "left,summary,summary,current" {
		t.Fatalf("replay crossed an existing summary: %s", replay)
	}
}
