package resume

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/accounts"
	"github.com/felinics/memoh/internal/agent/admit"
	"github.com/felinics/memoh/internal/agent/decision"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	"github.com/felinics/memoh/internal/bots"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

func (w *Worker) renewResumeCredential(ctx context.Context, botID string, claims map[string]string) (string, error) {
	if len(claims) == 0 {
		return "", nil
	}
	if claims["typ"] == "chat_route" {
		if claims["bot_id"] != botID || claims["chat_id"] == "" {
			return "", fmt.Errorf("%w: resume credential scope mismatch", runreq.ErrResumeUnrecoverable)
		}
	} else {
		accountID := claims["user_id"]
		if accountID == "" {
			accountID = claims["sub"]
		}
		if err := w.accounts.ValidateSession(ctx, accountID); err != nil {
			if errors.Is(err, accounts.ErrInactiveAccount) {
				return "", fmt.Errorf("%w: %w", runreq.ErrResumeUnrecoverable, err)
			}
			return "", err
		}
		allowed, err := w.permissions.HasBotPermission(ctx, botID, accountID, bots.PermissionChat)
		if err != nil {
			return "", err
		}
		if !allowed {
			return "", fmt.Errorf("%w: resume permission revoked", runreq.ErrResumeUnrecoverable)
		}
	}
	return w.signer.Renew(claims)
}

// Start probes the scope catalog before launching a bounded worker.
// The worker retains a cursor per current tenant instead of querying tenant data
// from the startup context. Run contexts retain the same binding after detaching.
func (w *Worker) Start(ctx context.Context) error {
	scopes := w.teamScopes
	first, err := scopes.ListScopePage(ctx, "", 16)
	if err != nil {
		return fmt.Errorf("list session resume scopes: %w", err)
	}
	if err := runreq.ValidateTeamScopePage(first, ""); err != nil {
		return err
	}
	workerCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	w.stop = cancel
	w.done = make(chan struct{})
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		scan := Scan{page: first, loaded: true}
		slots := make(chan struct{}, 4)
		for {
			if workerCtx.Err() != nil {
				return
			}
			if err := w.ScanInterrupted(workerCtx, &scan, slots); err != nil && workerCtx.Err() == nil {
				w.logger.WarnContext(workerCtx, "scan interrupted sessions failed", slog.Any("error", err))
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

// Scan is the cursor of a pass over the team scopes and their interrupted
// runs. The zero value starts at the first scope.
type Scan struct {
	page        runreq.TeamScopePage
	loaded      bool
	scopeCursor string
	scopeIndex  int
	runCursor   pgtype.UUID
}

// ScanInterrupted advances the scan by one step: a page of scopes, or a page of
// one scope's interrupted runs. It resumes as many of them as slots has room
// for and stops, keeping its place in scan, when the slots are full.
func (w *Worker) ScanInterrupted(ctx context.Context, scan *Scan, slots chan struct{}) error {
	scopes := w.teamScopes
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
			if retired, err := w.queries.RetireSupersededInterruptedSessionRuns(scopeCtx); err != nil {
				w.logger.WarnContext(scopeCtx, "retire superseded interrupted sessions failed", slog.Any("error", err))
			} else if retired > 0 {
				w.logger.InfoContext(scopeCtx, "retired superseded interrupted sessions", slog.Int64("count", retired))
			}
			scan.runCursor = pgtype.UUID{Valid: true}
		}
		rows, err := w.queries.ListInterruptedSessionRuns(scopeCtx, scan.runCursor)
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
			select {
			case slots <- struct{}{}:
				done, startErr := w.Resume(scopeCtx, row)
				switch {
				case errors.Is(startErr, runreq.ErrResumeUnrecoverable):
					w.retireInterruptedSession(scopeCtx, row, startErr)
				case startErr != nil:
					w.logger.WarnContext(scopeCtx, "resume interrupted session deferred", slog.String("run_id", row.RunID.String()), slog.Any("error", startErr))
				}
				if done == nil {
					<-slots
				} else {
					go func() { <-done; <-slots }()
				}
			default:
				return nil
			}
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

// Stop stops discovery only. Active runs are interrupted by the
// manager after it persists their marker, not by cancellation of this worker.
func (w *Worker) Stop() {
	if w.stop != nil {
		w.stop()
	}
	if w.done != nil {
		<-w.done
	}
}

// retireInterruptedSession drops a resume intent that can never continue, so
// later scans neither list nor retry it. The run itself stays lost.
func (w *Worker) retireInterruptedSession(ctx context.Context, row sqlc.SessionRun, cause error) {
	if _, err := w.queries.RetireInterruptedSessionRun(ctx, row.RunID); err != nil {
		w.logger.WarnContext(ctx, "retire interrupted session failed", slog.String("run_id", row.RunID.String()), slog.Any("error", err))
		return
	}
	w.logger.InfoContext(ctx, "interrupted session will not resume", slog.String("run_id", row.RunID.String()), slog.Any("reason", cause))
}

const Instruction = "The server gracefully shut down during the previous run. Continue the unfinished user task using the saved session history and current workspace. Do not repeat completed work. A tool interrupted before its result was saved may already have produced side effects: inspect files, process/job status and saved output before deciding what to do; do not blindly replay it. Background task handles from the previous process are no longer live; consult subagent session histories and external job receipts. If the task is already complete, report that. Original task: "

// probeReady checks the workspace transport the continuation will run on,
// within a bound of its own.
func (w *Worker) probeReady(ctx context.Context, botID, targetID string) error {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return w.ready(probeCtx, botID, targetID)
}

// Resume continues one interrupted run as a run of its own and reports, on the
// returned channel, when it has ended.
func (w *Worker) Resume(ctx context.Context, row sqlc.SessionRun) (<-chan struct{}, error) {
	if row.TeamID.String() != w.teamScopes.CurrentScope(ctx) {
		return nil, runreq.ErrTeamScopeMismatch
	}
	var input struct {
		Resume                *admit.ResumeContext                       `json:"resume"`
		DecisionContinuations map[string]decision.ContinuationCheckpoint `json:"decision_continuations"`
		CoverPosition         *int64                                     `json:"cover_position"`
	}
	if err := json.Unmarshal(row.InputJson, &input); err != nil {
		return nil, fmt.Errorf("%w: %w", runreq.ErrResumeUnrecoverable, err)
	}
	data := input.Resume
	if data == nil || data.Version != 1 || data.ChatID == "" {
		return nil, fmt.Errorf("%w: unsupported resume context", runreq.ErrResumeUnrecoverable)
	}
	if data.DeadlineAt != nil && !time.Now().Before(*data.DeadlineAt) {
		return nil, fmt.Errorf("%w: execution budget expired", runreq.ErrResumeUnrecoverable)
	}
	decisionContext, err := w.decider.InterruptedContext(ctx, row, input.DecisionContinuations)
	if err != nil {
		return nil, err
	}
	if err := w.probeReady(ctx, row.BotID.String(), data.WorkspaceTargetID); err != nil {
		return nil, err
	}
	token, err := w.renewResumeCredential(ctx, row.BotID.String(), data.TokenClaims)
	if err != nil {
		return nil, err
	}
	chatToken, err := w.renewResumeCredential(ctx, row.BotID.String(), data.ChatClaims)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	runBase, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if data.DeadlineAt != nil {
		var cancelDeadline context.CancelFunc
		runBase, cancelDeadline = context.WithDeadline(runBase, *data.DeadlineAt)
		oldCancel := cancel
		cancel = func() { cancelDeadline(); oldCancel() }
	}
	// A discuss run's cover position travels with its continuation, so the
	// batch it was answering is covered when the continuation completes.
	payloadFields := map[string]any{"kind": "resume", "interrupted_run_id": row.RunID.String(), "resume": data}
	if input.CoverPosition != nil {
		payloadFields["cover_position"] = *input.CoverPosition
	}
	payload, err := json.Marshal(payloadFields)
	if err != nil {
		cancel()
		return nil, err
	}
	runCtx, admission, _, err := w.admit.Triggered(runBase, admit.Trigger{
		BotID: row.BotID.String(), ThreadID: row.SessionID.String(), InvocationID: "resume:" + row.RunID.String(),
		Submission: payload, StopCause: outcome.ErrRunStopped, ResumeRunID: row.RunID.String(),
	})
	if err != nil {
		cancel()
		if errors.Is(err, ledger.ErrResumeSuperseded) || errors.Is(err, sessionruntime.ErrInvocationConflict) {
			// A newer turn answered the session, or another worker already
			// admitted this continuation.
			return nil, fmt.Errorf("%w: %w", runreq.ErrResumeUnrecoverable, err)
		}
		return nil, err
	}
	// The interrupted turn's request row is already in history, and the
	// instruction below carries the original task again. Naming the row lets
	// context assembly drop the history copy so the model sees the task once.
	// Without a resolvable row (older records, lookup failure) both stay.
	requestMessageID, lookupErr := w.transcript.ContinuationRequestMessageID(ctx, row.SessionID.String(), sessionruntime.RunHandle{TurnID: runreq.PGUUIDString(row.TurnID)})
	if lookupErr != nil {
		w.logger.WarnContext(runCtx, "resume request message lookup failed", slog.String("run_id", admission.RunID), slog.Any("error", lookupErr))
		requestMessageID = ""
	}
	req := runreq.ChatRequest{
		BotID: row.BotID.String(), ThreadID: row.SessionID.String(), ChatID: data.ChatID,
		PersistedUserMessageID: requestMessageID,
		RunID:                  admission.RunID, RunHandle: admission.Handle, TurnID: admission.TurnID, TurnPosition: &admission.TurnPosition,
		Query: Instruction + data.Query, UserID: data.UserID, SourceChannelIdentityID: data.ChannelIdentityID,
		Token: token, ChatToken: chatToken, CurrentChannel: data.CurrentChannel, ReplyTarget: data.ReplyTarget,
		ConversationType: data.ConversationType, Model: data.Model, ReasoningEffort: data.ReasoningEffort,
		WorkspaceTargetID: data.WorkspaceTargetID, SessionType: data.SessionType, ToolHTTPURL: data.ToolHTTPURL,
		UserMessagePersisted: true, SkipMemoryExtraction: true, SkipTitleGeneration: true, ShutdownResume: true,
	}
	if decisionContext != "" {
		req.Query += "\n\n" + decisionContext
	}

	spec := drive.RunSpec{
		Run: drive.AdmittedRun{
			Handle:       admission.Handle,
			TurnID:       admission.TurnID,
			TurnPosition: &admission.TurnPosition,
			Mode:         data.SessionType,
		},
		Req:      req,
		Delivery: drive.NoDelivery{},
		Input:    outcome.InputSynthetic,
	}
	spec.Setup = func(ctx context.Context, req runreq.ChatRequest) (drive.RunPlan, error) {
		return w.plan.ChatSetup(ctx, req, spec)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		w.driver.Drive(runCtx, spec)
	}()

	w.logger.InfoContext(runCtx, "interrupted session resumed", slog.String("previous_run_id", row.RunID.String()), slog.String("run_id", admission.RunID), slog.String("session_id", req.ThreadID))
	return done, nil
}
