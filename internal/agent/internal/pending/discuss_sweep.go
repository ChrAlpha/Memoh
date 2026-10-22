package pending

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

const (
	// DiscussSweepInterval is how often the sweep looks for discuss input that
	// nothing has answered. Admission is idempotent, so the interval trades
	// only latency for load.
	DiscussSweepInterval = time.Minute
	// DiscussSweepWindow bounds how far back the sweep looks. It has to outlast
	// a server restart or a deploy, since input that arrived while no server
	// could admit it is what the sweep exists for. Input older than that is
	// left alone rather than answered long after the conversation moved on.
	DiscussSweepWindow = 30 * time.Minute
)

// DiscussSweep is the backstop for the two prompt triggers of a discuss
// admission, an input being committed and a run ending. It asks the admission
// about every discuss session that took external input within the window, once
// at start and then on every interval.
//
// Several servers may sweep at once. Which of them admits a batch is decided by
// the admission's invocation id and the session's single slot, as for any
// other caller.
type DiscussSweep struct {
	inputs *Inputs
	stop   context.CancelFunc
	done   chan struct{}
}

// NewDiscussSweep returns the sweep over inputs' discuss sessions.
func NewDiscussSweep(inputs *Inputs) *DiscussSweep {
	return &DiscussSweep{inputs: inputs}
}

// Start runs the first sweep immediately and the following ones on the
// interval, until Stop. The sweep outlives ctx: the discuss runs it admits are
// stopped by the runtime manager on shutdown, not by this worker.
func (d *DiscussSweep) Start(ctx context.Context) error {
	workerCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	d.stop = stop
	d.done = make(chan struct{})
	ticker := time.NewTicker(DiscussSweepInterval)
	go func() {
		defer close(d.done)
		defer ticker.Stop()
		d.Run(workerCtx, ticker.C, time.Now)
	}()
	return nil
}

// Stop ends discovery. Runs already admitted are not touched.
func (d *DiscussSweep) Stop() {
	if d.stop != nil {
		d.stop()
	}
	if d.done != nil {
		<-d.done
	}
}

// Run sweeps once, then once per tick, until ctx ends. The clock and the ticks
// are the caller's so a test can drive both.
func (d *DiscussSweep) Run(ctx context.Context, ticks <-chan time.Time, now func() time.Time) {
	for {
		d.Sweep(ctx, now())
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

// Sweep checks, in every database scope, each discuss session whose newest
// external input within the window is not covered yet.
func (d *DiscussSweep) Sweep(ctx context.Context, now time.Time) {
	d.inputs.eachScope(ctx, "discuss sweep", func(scopeCtx context.Context, _ string) {
		d.sweepScope(scopeCtx, now.Add(-DiscussSweepWindow))
	})
}

func (d *DiscussSweep) sweepScope(ctx context.Context, since time.Time) {
	in := d.inputs
	candidates, err := in.queries.ListDiscussSweepCandidates(ctx, dbsqlc.ListDiscussSweepCandidatesParams{
		Since:  pgtype.Timestamptz{Time: since, Valid: true},
		States: CoverStates,
	})
	if err != nil {
		in.logger.WarnContext(ctx, "list discuss sweep candidates failed", slog.Any("error", err))
		return
	}
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return
		}
		botID, sessionID := candidate.BotID.String(), candidate.SessionID.String()
		// The run, if one is admitted, belongs to the session and not to this
		// pass.
		outcome, err := in.CheckAndAdmitDiscuss(context.WithoutCancel(ctx), botID, sessionID)
		switch {
		case err == nil && outcome == DiscussAdmitted:
			in.logger.InfoContext(ctx, "discuss sweep admitted a batch", slog.String("session_id", sessionID))
		case err != nil && !errors.Is(err, sessionruntime.ErrManagerClosed):
			in.logger.WarnContext(ctx, "discuss sweep check failed", slog.String("session_id", sessionID), slog.Any("error", err))
		}
	}
}
