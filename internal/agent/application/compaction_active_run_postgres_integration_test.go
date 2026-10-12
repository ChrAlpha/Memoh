package application

import (
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/context/compaction"
)

// A pass that runs while the session's run is still writing leaves that run's
// rows raw, even its older steps a pass would otherwise claim behind the task;
// once the run is terminal the same pass claims them.
func TestPostgresCompactionLeavesTheActiveRunsTurnRaw(t *testing.T) {
	f, store := newProgressFixture(t)
	task := f.text("user", "ACTIVE TASK "+strings.Repeat("do the work ", 40))
	var steps []string
	for n := 1; n <= 6; n++ {
		for _, msg := range f.exchange("exec", strconv.Itoa(n), strings.Repeat("tool output ", 200)) {
			steps = append(steps, msg.ID)
		}
	}
	var turnID string
	var position int64
	if err := f.pool.QueryRow(f.ctx, `SELECT turn_id::text, turn_position FROM bot_history_messages WHERE id = $1`, task.ID).Scan(&turnID, &position); err != nil {
		t.Fatal(err)
	}
	runID := uuid.NewString()
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO session_runs (
			run_id, bot_id, session_id, invocation_id, turn_id, turn_position, state, input_json, input_fingerprint
		) VALUES ($1, $2, $3, $4, $5, $6, 'running', '{}'::jsonb, 'fingerprint')`,
		runID, f.botID, f.sessionID, uuid.NewString(), turnID, position); err != nil {
		t.Fatal(err)
	}

	model := &countingSummarizer{summary: strings.Repeat("summary ", 30)}
	cfg := f.config(model)
	svc := compaction.NewService(slog.New(slog.DiscardHandler), store)
	res, err := svc.RunCompactionSync(f.ctx, cfg)
	if err != nil || res.Status != compaction.StatusNoop || model.calls != 0 {
		t.Fatalf("pass during the run = %+v, %v after %d calls; want a noop that calls no model", res, err, model.calls)
	}
	for _, id := range append([]string{task.ID}, steps...) {
		if claim, _ := f.claim(id); claim != "" {
			t.Fatalf("the active run's row %s was claimed: %s", id, claim)
		}
	}

	if _, err := f.pool.Exec(f.ctx, `UPDATE session_runs SET state = 'completed' WHERE run_id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	res, err = svc.RunCompactionSync(f.ctx, cfg)
	if err != nil || res.Status != compaction.StatusOK || model.calls != 1 {
		t.Fatalf("pass after the run = %+v, %v after %d calls; want the run's older steps claimed", res, err, model.calls)
	}
	claimed := 0
	for _, id := range steps {
		if claim, _ := f.claim(id); claim != "" {
			claimed++
		}
	}
	if claim, _ := f.claim(task.ID); claimed == 0 || claim != "" {
		t.Fatalf("after the run: %d steps claimed, task claim %q; want older steps claimed and the task raw", claimed, claim)
	}
}
