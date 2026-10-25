package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel"

	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// StepOverLimits bound what a request renders of the history its bot
// recorded before keeping history under checkpoints.
type StepOverLimits struct {
	// ContentBytes is the content a request loads after its checkpoint, the
	// byte budget of its history load. Zero steps over nothing.
	ContentBytes int64
	// RowPayloadBytes, when positive, is the most one compaction pass reads
	// of a row: an earlier row larger than that could never be folded.
	RowPayloadBytes int64
}

// StepOver makes a checkpoint stand, without a summary, for the history the
// session's bot recorded before it kept history under checkpoints that a
// request cannot render (bots.history_checkpoints_since). Walking that
// history newest first, as the history load measures content, it omits the
// turns past limits.ContentBytes, or from a row one compaction pass could not
// read, through the oldest such turn after the active checkpoint, and the
// earlier rows that checkpoint keeps by reference past the same bound. It
// keeps everything else, and records what it omits in Omitted.
//
// Only that earlier history is ever omitted, by whole turns that began before
// the boundary: a session created since never steps over, and the earlier
// history a request rendered stays inside the bound, so it is summarized when
// it no longer fits, never dropped. Past the bound is what no request since
// then could render. before, when set, bounds the history as an edit or
// retry's cutoff does. StepOver reports how many context rows it omitted:
// none when nothing needs stepping over, or when the session's checkpoint
// moved meanwhile.
func (s *Store) StepOver(ctx context.Context, sessionID string, before *messagepkg.HistoryPosition, limits StepOverLimits) (int64, error) {
	if limits.ContentBytes <= 0 {
		return 0, nil
	}
	pgSessionID, err := dbpkg.ParseUUID(sessionID)
	if err != nil {
		return 0, err
	}
	boundary, err := s.queries.GetHistoryCheckpointStepOverBoundary(ctx, pgSessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("load history checkpoint boundary: %w", err)
	}
	if !boundary.HasPreCheckpointRows {
		return 0, nil
	}
	base, err := s.Active(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	var (
		plan    stepOverPlan
		created Checkpoint
	)
	err = s.inTx(ctx, func(q dbstore.Queries) error {
		active, err := q.LockHistoryCheckpointSession(ctx, sqlc.LockHistoryCheckpointSessionParams{SessionID: pgSessionID, BotID: boundary.BotID})
		if err != nil {
			return fmt.Errorf("lock history checkpoint session: %w", err)
		}
		if active != dbpkg.ParseUUIDOrEmpty(base.ID) {
			return ErrMoved
		}
		plan, err = planStepOver(ctx, q, pgSessionID, base, before, boundary.HistoryCheckpointsSince, limits)
		if err != nil || plan.rows == 0 && plan.droppedRows == 0 {
			return err
		}
		created, err = create(ctx, q, pgSessionID, base, plan.position, plan.omitted, plan.items)
		return err
	})
	if errors.Is(err, ErrMoved) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("step over history before checkpoints: %w", err)
	}
	if created.ID == "" {
		return 0, nil
	}
	omitted := plan.rows + plan.droppedRows
	s.logger.InfoContext(ctx, "history checkpoint stepped over history from before checkpoints",
		slog.String("session_id", sessionID),
		slog.String("history_checkpoint_id", created.ID),
		slog.String("parent_history_checkpoint_id", base.ID),
		slog.Int64("rows", plan.rows),
		slog.Int64("kept_rows_dropped", plan.droppedRows),
		slog.Time("first_at", created.Omitted.First),
		slog.Time("last_at", created.Omitted.Last),
		slog.Int64("content_bytes_limit", limits.ContentBytes),
		slog.Int64("row_payload_bytes_limit", limits.RowPayloadBytes))
	countSteppedOverRows(ctx, omitted)
	return omitted, nil
}

// stepOverPlan is the checkpoint a step-over writes.
type stepOverPlan struct {
	position messagepkg.HistoryPosition
	items    []Item
	omitted  Omitted
	// rows counts the context rows after the base it omits, droppedRows the
	// rows the base keeps by reference that it omits.
	rows        int64
	droppedRows int64
}

// planStepOver reads, in the transaction q runs in, what a step-over of the
// history before since omits after base, and returns the zero plan when it
// omits nothing.
func planStepOver(ctx context.Context, q dbstore.Queries, sessionID pgtype.UUID, base Checkpoint, before *messagepkg.HistoryPosition, since pgtype.Timestamptz, limits StepOverLimits) (stepOverPlan, error) {
	walkParams := sqlc.WalkPreCheckpointHistoryAfterParams{
		Since:               since,
		SessionID:           sessionID,
		AfterTurnPosition:   base.Position.Turn,
		AfterTurnMessageSeq: base.Position.Seq,
		MaxBytes:            limits.ContentBytes,
		MaxRowBytes:         limits.RowPayloadBytes,
	}
	if before != nil {
		walkParams.BeforeTurnPosition = pgtype.Int8{Int64: before.Turn, Valid: true}
		walkParams.BeforeTurnMessageSeq = pgtype.Int8{Int64: before.Seq, Valid: true}
	}
	walked, err := q.WalkPreCheckpointHistoryAfter(ctx, walkParams)
	if err != nil {
		return stepOverPlan{}, fmt.Errorf("walk history before checkpoints: %w", err)
	}
	stop := -1
	var total int64
	for i, row := range walked {
		total = row.TotalBytes
		if row.Pre && limits.past(row.TotalBytes, row.PayloadBytes) {
			stop = i
			break
		}
	}

	plan := stepOverPlan{position: base.Position, omitted: base.Omitted}
	if stop >= 0 {
		through := walked[stop].TurnPosition.Int64
		if before != nil && through >= before.Turn {
			through = before.Turn - 1
		}
		if err := plan.omitAfter(ctx, q, sessionID, base, through, since); err != nil {
			return stepOverPlan{}, err
		}
	}

	dropped, err := droppedKeptRows(ctx, q, sessionID, base, since, total, stop >= 0, limits)
	if err != nil {
		return stepOverPlan{}, err
	}
	plan.items = make([]Item, 0, len(base.Items))
	for _, item := range base.Items {
		if item.RowID != "" {
			if row, ok := dropped[item.RowID]; ok {
				plan.droppedRows++
				plan.omitted = plan.omitted.add(1, row, row)
				continue
			}
		}
		plan.items = append(plan.items, item)
	}
	if plan.rows == 0 && plan.droppedRows == 0 {
		return stepOverPlan{}, nil
	}
	return plan, nil
}

// past reports whether an earlier row takes the walked content past what a
// request renders, or alone exceeds what a compaction pass reads.
func (l StepOverLimits) past(totalBytes, payloadBytes int64) bool {
	return totalBytes > l.ContentBytes || l.RowPayloadBytes > 0 && payloadBytes > l.RowPayloadBytes
}

// omitAfter omits the context rows after base through the end of turn
// through. A turn that began since the boundary is never omitted, so the
// step-over then ends with the turn before the first such turn.
func (p *stepOverPlan) omitAfter(ctx context.Context, q dbstore.Queries, sessionID pgtype.UUID, base Checkpoint, through int64, since pgtype.Timestamptz) error {
	for through >= base.Position.Turn {
		measure, err := q.MeasureHistoryCheckpointStepOver(ctx, sqlc.MeasureHistoryCheckpointStepOverParams{
			ThroughTurnPosition: through,
			Since:               since,
			SessionID:           sessionID,
			AfterTurnPosition:   base.Position.Turn,
			AfterTurnMessageSeq: base.Position.Seq,
		})
		if err != nil {
			return fmt.Errorf("measure history step-over: %w", err)
		}
		if measure.LaterRows > 0 {
			through = measure.FirstLaterTurnPosition - 1
			continue
		}
		if measure.RowCount == 0 {
			return nil
		}
		p.position = messagepkg.HistoryPosition{Turn: through, Seq: measure.LastTurnMessageSeq}
		p.rows = measure.RowCount
		p.omitted = p.omitted.add(measure.RowCount, measure.FirstAt.Time, measure.LastAt.Time)
		return nil
	}
	return nil
}

// droppedKeptRows names the earlier rows base keeps by reference that a
// step-over omits, with when each was written: all of them when the history
// after base already passed the bound, otherwise those of the turns from the
// first one past it, walking on from the walked total. A turn's kept rows go
// together, so a tool exchange is never split.
func droppedKeptRows(ctx context.Context, q dbstore.Queries, sessionID pgtype.UUID, base Checkpoint, since pgtype.Timestamptz, walkedBytes int64, passed bool, limits StepOverLimits) (map[string]time.Time, error) {
	keptIDs := base.RowUUIDs()
	if len(keptIDs) == 0 {
		return nil, nil
	}
	params := sqlc.WalkPreCheckpointKeptRowsParams{
		Since:       since,
		SessionID:   sessionID,
		KeptIds:     keptIDs,
		StartBytes:  walkedBytes,
		MaxBytes:    limits.ContentBytes,
		MaxRowBytes: limits.RowPayloadBytes,
	}
	if passed {
		// Every earlier kept row is past the bound: walk none of them.
		params.MaxBytes = -1
	}
	rows, err := q.WalkPreCheckpointKeptRows(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("walk kept history before checkpoints: %w", err)
	}
	dropFrom := int64(-1)
	if passed {
		dropFrom = maxKeptTurn(rows)
	} else {
		for _, row := range rows {
			if row.Walked && row.Pre && limits.past(row.TotalBytes, row.PayloadBytes) {
				dropFrom = row.TurnPosition.Int64
				break
			}
		}
	}
	if dropFrom < 0 {
		return nil, nil
	}
	dropped := make(map[string]time.Time)
	for _, row := range rows {
		if row.Pre && row.TurnPosition.Int64 <= dropFrom {
			dropped[row.ID.String()] = row.CreatedAt.Time
		}
	}
	return dropped, nil
}

func maxKeptTurn(rows []sqlc.WalkPreCheckpointKeptRowsRow) int64 {
	turn := int64(-1)
	for _, row := range rows {
		turn = max(turn, row.TurnPosition.Int64)
	}
	return turn
}

// transactionRunner runs a function in one database transaction.
type transactionRunner interface {
	InTx(context.Context, func(dbstore.Queries) error) error
	SupportsTransactions() bool
}

func (s *Store) inTx(ctx context.Context, fn func(dbstore.Queries) error) error {
	if txer, ok := s.queries.(transactionRunner); ok && txer.SupportsTransactions() {
		return txer.InTx(ctx, fn)
	}
	return fn(s.queries)
}

func countSteppedOverRows(ctx context.Context, rows int64) {
	counter, err := otel.Meter(meterName).Int64Counter("history.checkpoint.stepped_over_rows_total")
	if err != nil {
		return
	}
	counter.Add(ctx, rows)
}
