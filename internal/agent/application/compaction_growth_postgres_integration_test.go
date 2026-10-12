package application

import (
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/context/compaction"
)

func TestPostgresCompactionGrowthRetainsProgressAcrossRestarts(t *testing.T) {
	const turns = 60
	f, store := newProgressFixture(t)
	model := &countingSummarizer{summary: strings.Repeat("s", 4*254-21)}
	cfg := f.config(model)
	svc := compaction.NewService(slog.New(slog.DiscardHandler), store)
	var outputs []string
	passes, ineffective := 0, 0
	run := func() {
		calls := model.calls
		_, err := svc.RunCompactionSync(f.ctx, cfg)
		if err != nil && !errors.Is(err, compaction.ErrIneffectiveSummary) {
			t.Fatal(err)
		}
		if err != nil {
			ineffective++
		}
		if model.calls-calls > 1 {
			t.Fatal("one scan made multiple provider requests")
		}
		passes++
	}
	for n := 1; n <= turns; n++ {
		if n%15 == 0 {
			svc = compaction.NewService(slog.New(slog.DiscardHandler), store)
		}
		f.text("user", "task "+strconv.Itoa(n))
		if n%5 == 0 {
			f.reasoning()
		}
		rows := f.exchange("exec", strconv.Itoa(n), strings.Repeat("step completed. ", 150))
		outputs = append(outputs, rows[1].ID)
		current := f.text("user", "current question")
		for pass := 0; pass < 3; pass++ {
			model.summary = strings.Repeat("s", 4*254-21)
			if n%7 == 0 && pass == 0 {
				model.summary = strings.Repeat("s", 16000)
			}
			run()
			if _, status := f.claim(current.ID); status == "ok" {
				t.Fatal("current question was summarized")
			}
		}
	}
	model.summary = strings.Repeat("s", 4*254-21)
	for n := 0; n < 2*turns+10; n++ {
		var remaining int
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM bot_visible_history_messages m
			LEFT JOIN bot_history_message_compacts c ON c.id=m.compact_id
			WHERE m.id=ANY($1::uuid[]) AND c.status IS DISTINCT FROM 'ok'`, outputs).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining == 0 {
			t.Logf("turns=%d restarts=4 outputs_committed=%d passes=%d provider_calls=%d ineffective=%d", turns, len(outputs), passes, model.calls, ineffective)
			return
		}
		run()
	}
	t.Fatal("old short rows prevented later tool outputs from eventually compacting")
}
