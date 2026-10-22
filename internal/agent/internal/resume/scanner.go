package resume

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/agentapi/ports"
	"github.com/felinics/memoh/internal/agent/internal/pending"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// scanInterval is how often the scanner looks for sessions owed a continuation.
const scanInterval = 5 * time.Second

// ScannerQueries is the database port of the interrupted-run scan.
type ScannerQueries interface {
	ListInterruptedSessionRuns(context.Context, pgtype.UUID) ([]dbsqlc.SessionRun, error)
	RetireSupersededInterruptedSessionRuns(context.Context) (int64, error)
}

// ScannerDeps is everything NewScanner needs; all of it is required.
type ScannerDeps struct {
	// Logger receives the scanner's own records.
	Logger *slog.Logger
	// Queries lists the interrupted runs and retires superseded ones.
	Queries ScannerQueries
	// Pending is woken for each session that is owed a continuation; it decides
	// whether and when the continuation starts.
	Pending *pending.Inputs
	// TeamScopes names the scopes the scanner covers.
	TeamScopes ports.TeamScopes
}

// Scanner finds the sessions owed a continuation and wakes pending for them.
// It starts nothing itself: the one admission of what waits on a session is
// pending's.
type Scanner struct {
	logger     *slog.Logger
	queries    ScannerQueries
	pending    *pending.Inputs
	teamScopes ports.TeamScopes
	stop       context.CancelFunc
	done       chan struct{}
}

// NewScanner returns a Scanner.
func NewScanner(d ScannerDeps) *Scanner {
	return &Scanner{logger: d.Logger, queries: d.Queries, pending: d.Pending, teamScopes: d.TeamScopes}
}

// Start probes the scope catalog before launching a bounded worker.
// The worker retains a cursor per current tenant instead of querying tenant data
// from the startup context. Run contexts retain the same binding after detaching.
func (s *Scanner) Start(ctx context.Context) error {
	first, err := s.teamScopes.ListScopePage(ctx, "", 16)
	if err != nil {
		return fmt.Errorf("list session resume scopes: %w", err)
	}
	if err := runreq.ValidateTeamScopePage(first, ""); err != nil {
		return err
	}
	workerCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.stop = cancel
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(scanInterval)
		defer ticker.Stop()
		scan := Scan{page: first, loaded: true}
		for {
			if workerCtx.Err() != nil {
				return
			}
			if err := s.ScanInterrupted(workerCtx, &scan); err != nil && workerCtx.Err() == nil {
				s.logger.WarnContext(workerCtx, "scan interrupted sessions failed", slog.Any("error", err))
			}
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return nil
}

// Stop stops discovery only. Active runs are interrupted by the
// manager after it persists their marker, not by cancellation of this worker.
func (s *Scanner) Stop() {
	if s.stop != nil {
		s.stop()
	}
	if s.done != nil {
		<-s.done
	}
}

// Scan is the cursor of a pass over the team scopes and their interrupted
// runs. The zero value starts at the first scope.
type Scan struct {
	page        ports.TeamScopePage
	loaded      bool
	scopeCursor string
	scopeIndex  int
	runCursor   pgtype.UUID
}

// ScanInterrupted advances the scan by one step: a page of scopes, or a page of
// one scope's interrupted runs, waking pending for the session of each.
func (s *Scanner) ScanInterrupted(ctx context.Context, scan *Scan) error {
	scopes := s.teamScopes
	if !scan.loaded {
		page, err := scopes.ListScopePage(ctx, scan.scopeCursor, 16)
		if err != nil {
			return err
		}
		if err := runreq.ValidateTeamScopePage(page, scan.scopeCursor); err != nil {
			return err
		}
		scan.page, scan.loaded = page, true
		scan.scopeIndex = 0
	}
	for scan.scopeIndex < len(scan.page.Scopes) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		scopeCtx, err := runreq.BindTeamScope(ctx, scopes, scan.page.Scopes[scan.scopeIndex])
		if err != nil {
			scan.scopeIndex++
			scan.runCursor = pgtype.UUID{}
			return err
		}
		if !scan.runCursor.Valid {
			// Each pass over a scope starts by retiring intents a later turn or
			// a session deletion has made moot, so the pending-resume index and
			// the listing below only carry work that can still continue.
			if retired, err := s.queries.RetireSupersededInterruptedSessionRuns(scopeCtx); err != nil {
				s.logger.WarnContext(scopeCtx, "retire superseded interrupted sessions failed", slog.Any("error", err))
			} else if retired > 0 {
				s.logger.InfoContext(scopeCtx, "retired superseded interrupted sessions", slog.Int64("count", retired))
			}
			scan.runCursor = pgtype.UUID{Valid: true}
		}
		rows, err := s.queries.ListInterruptedSessionRuns(scopeCtx, scan.runCursor)
		if err != nil {
			scan.scopeIndex++
			scan.runCursor = pgtype.UUID{}
			return err
		}
		for _, row := range rows {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if row.TeamID.String() != scopes.CurrentScope(scopeCtx) {
				return runreq.ErrTeamScopeMismatch
			}
			s.pending.Kick(scopeCtx, row.BotID.String(), row.SessionID.String())
			scan.runCursor = row.RunID
		}
		if len(rows) == 100 {
			return nil
		}
		scan.scopeIndex++
		scan.runCursor = pgtype.UUID{}
	}
	scan.scopeCursor = scan.page.NextCursor
	if scan.page.Complete {
		scan.scopeCursor = ""
	}
	scan.loaded = false
	return nil
}
