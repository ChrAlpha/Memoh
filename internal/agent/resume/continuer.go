// Package resume continues the runs a server shutdown interrupted. The
// Continuer says which continuation a session is owed and starts it, renewing
// the credentials the interrupted run carried; pending decides when. The
// Scanner looks through each team scope for sessions that are owed one and
// wakes pending for them.
package resume

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/accounts"
	"github.com/felinics/memoh/internal/agent/admit"
	"github.com/felinics/memoh/internal/agent/decide"
	"github.com/felinics/memoh/internal/agent/decision"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/pending"
	"github.com/felinics/memoh/internal/agent/plan"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	"github.com/felinics/memoh/internal/agent/transcript"
	"github.com/felinics/memoh/internal/bots"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// maxRunningContinuations bounds the continuations one process has running at
// once. After a rolling deploy every run the old instance had in flight is owed
// one, and starting them all together would hit the model and the workspaces in
// one burst; the sessions that find no slot wait for the next trigger.
const maxRunningContinuations = 4

// ContinuerQueries is the database port of the continuation.
type ContinuerQueries interface {
	GetSessionInterruptedRun(context.Context, pgtype.UUID) (dbsqlc.SessionRun, error)
	RetireInterruptedSessionRun(context.Context, pgtype.UUID) (int64, error)
}

// Accounts says whether the account a resumed credential names may still act.
type Accounts interface {
	ValidateSession(ctx context.Context, userID string) error
}

// ContinuerDeps is everything NewContinuer needs; all of it is required.
type ContinuerDeps struct {
	// Logger receives the continuer's own records.
	Logger *slog.Logger
	// Queries finds and retires a session's interrupted run.
	Queries ContinuerQueries
	// Accounts validates the account behind a renewed credential.
	Accounts Accounts
	// BotPermissions decides whether that account may still chat with the bot.
	BotPermissions runreq.BotPermissionChecker
	// Signer renews the credentials of a resumed run.
	Signer *admit.Signer
	// Admit admits the continuation.
	Admit *admit.Admitter
	// Decider says what the interrupted run's open decisions were.
	Decider *decide.Decider
	// Transcript finds the request row the continuation answers.
	Transcript *transcript.Reader
	// Driver executes the continuation.
	Driver *drive.Driver
	// Plan plans the continuation.
	Plan *plan.Planner
	// TeamScopes names the scope a continuation must run in.
	TeamScopes runreq.TeamScopes
}

// Continuer implements pending.Continuations: it knows what an interrupted run
// left to continue and how to continue it.
type Continuer struct {
	logger      *slog.Logger
	queries     ContinuerQueries
	accounts    Accounts
	permissions runreq.BotPermissionChecker
	signer      *admit.Signer
	admit       *admit.Admitter
	decider     *decide.Decider
	transcript  *transcript.Reader
	driver      *drive.Driver
	plan        *plan.Planner
	teamScopes  runreq.TeamScopes
	// slots holds a token for every continuation run in flight.
	slots chan struct{}
}

var _ pending.Continuations = (*Continuer)(nil)

// NewContinuer returns a Continuer.
func NewContinuer(d ContinuerDeps) *Continuer {
	return &Continuer{
		logger:      d.Logger,
		queries:     d.Queries,
		accounts:    d.Accounts,
		permissions: d.BotPermissions,
		signer:      d.Signer,
		admit:       d.Admit,
		decider:     d.Decider,
		transcript:  d.Transcript,
		driver:      d.Driver,
		plan:        d.Plan,
		teamScopes:  d.TeamScopes,
		slots:       make(chan struct{}, maxRunningContinuations),
	}
}

// intent is what an interrupted run recorded to be continued from.
type intent struct {
	Resume                *admit.ResumeContext                       `json:"resume"`
	DecisionContinuations map[string]decision.ContinuationCheckpoint `json:"decision_continuations"`
	CoverPosition         *int64                                     `json:"cover_position"`
}

// readIntent parses the intent of a run and checks that it can still be
// continued: a format this binary reads, and an execution budget not yet spent.
func readIntent(row dbsqlc.SessionRun) (intent, *admit.ResumeContext, error) {
	var in intent
	if err := json.Unmarshal(row.InputJson, &in); err != nil {
		return intent{}, nil, fmt.Errorf("%w: %w", runreq.ErrResumeUnrecoverable, err)
	}
	data := in.Resume
	if data == nil || data.Version != 1 || data.ChatID == "" {
		return intent{}, nil, fmt.Errorf("%w: unsupported resume context", runreq.ErrResumeUnrecoverable)
	}
	if data.DeadlineAt != nil && !time.Now().Before(*data.DeadlineAt) {
		return intent{}, nil, fmt.Errorf("%w: execution budget expired", runreq.ErrResumeUnrecoverable)
	}
	return in, data, nil
}

// Owed returns the continuation the session is owed.
func (c *Continuer) Owed(ctx context.Context, key sessionruntime.Key) (pending.Continuation, bool, error) {
	sessionID, err := db.ParseUUID(key.SessionID)
	if err != nil {
		return pending.Continuation{}, false, err
	}
	row, err := c.queries.GetSessionInterruptedRun(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return pending.Continuation{}, false, nil
	}
	if err != nil {
		return pending.Continuation{}, false, err
	}
	owed := pending.Continuation{Run: row}
	_, data, err := readIntent(row)
	if err != nil {
		return owed, true, err
	}
	owed.WorkspaceTargetID = data.WorkspaceTargetID
	return owed, true, nil
}

// Retire drops the intent of a run that can never continue, so that it is not
// owed again. The run itself stays lost.
func (c *Continuer) Retire(ctx context.Context, owed pending.Continuation, _ error) error {
	_, err := c.queries.RetireInterruptedSessionRun(ctx, owed.Run.RunID)
	return err
}

func (c *Continuer) renewResumeCredential(ctx context.Context, botID string, claims map[string]string) (string, error) {
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
		if err := c.accounts.ValidateSession(ctx, accountID); err != nil {
			if errors.Is(err, accounts.ErrInactiveAccount) {
				return "", fmt.Errorf("%w: %w", runreq.ErrResumeUnrecoverable, err)
			}
			return "", err
		}
		allowed, err := c.permissions.HasBotPermission(ctx, botID, accountID, bots.PermissionChat)
		if err != nil {
			return "", err
		}
		if !allowed {
			return "", fmt.Errorf("%w: resume permission revoked", runreq.ErrResumeUnrecoverable)
		}
	}
	return c.signer.Renew(claims)
}

// Instruction is what a continuation tells the model about why it sees the
// original task again.
const Instruction = "The server gracefully shut down during the previous run. Continue the unfinished user task using the saved session history and current workspace. Do not repeat completed work. A tool interrupted before its result was saved may already have produced side effects: inspect files, process/job status and saved output before deciding what to do; do not blindly replay it. Background task handles from the previous process are no longer live; consult subagent session histories and external job receipts. If the task is already complete, report that. Original task: "

// Start continues one interrupted run as a run of its own and reports, on the
// returned channel, when it has ended. It holds one of the continuation slots
// from the call until the run has ended, or until it fails; with none free it
// returns pending.ErrContinuationsBusy.
func (c *Continuer) Start(ctx context.Context, owed pending.Continuation) (<-chan struct{}, error) {
	select {
	case c.slots <- struct{}{}:
	default:
		return nil, pending.ErrContinuationsBusy
	}
	done, err := c.start(ctx, owed)
	if err != nil {
		<-c.slots
		return nil, err
	}
	go func() { <-done; <-c.slots }()
	return done, nil
}

func (c *Continuer) start(ctx context.Context, owed pending.Continuation) (<-chan struct{}, error) {
	row := owed.Run
	if row.TeamID.String() != c.teamScopes.CurrentScope(ctx) {
		return nil, runreq.ErrTeamScopeMismatch
	}
	input, data, err := readIntent(row)
	if err != nil {
		return nil, err
	}
	decisionContext, err := c.decider.InterruptedContext(ctx, row, input.DecisionContinuations)
	if err != nil {
		return nil, err
	}
	token, err := c.renewResumeCredential(ctx, row.BotID.String(), data.TokenClaims)
	if err != nil {
		return nil, err
	}
	chatToken, err := c.renewResumeCredential(ctx, row.BotID.String(), data.ChatClaims)
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
	// The interrupted turn's request row is already in history, and the
	// continuation's steps are filed under it. The run answers that row, so it
	// joins the turn the row belongs to: the interrupted run's root turn, or the
	// later turn an applied steer opened. When the saved query restates the task
	// in the instruction below, context assembly drops the history copy so the
	// model sees the task once; a run whose task is only that row (a discuss
	// batch) keeps it. Without a resolvable row (older records, lookup failure)
	// the steps cannot be filed under it, both copies stay, and the run joins
	// its root turn.
	join := ledger.Turn{ID: runreq.PGUUIDString(row.TurnID), Position: row.TurnPosition}
	request, lookupErr := c.transcript.ContinuationRequest(ctx, row.SessionID.String(), sessionruntime.RunHandle{RunID: row.RunID.String(), TurnID: join.ID})
	if lookupErr != nil {
		c.logger.WarnContext(ctx, "resume request message lookup failed", slog.String("run_id", row.RunID.String()), slog.Any("error", lookupErr))
		request = messagepkg.RunTurnRequest{}
	}
	if request.TurnID != "" {
		join = ledger.Turn{ID: request.TurnID, Position: request.TurnPosition}
	}
	runCtx, admission, _, err := c.admit.Triggered(runBase, admit.Trigger{
		BotID: row.BotID.String(), ThreadID: row.SessionID.String(), InvocationID: "resume:" + row.RunID.String(),
		Submission: payload, StopCause: outcome.ErrRunStopped, ResumeRunID: row.RunID.String(),
		JoinTurn: &join,
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
	req := runreq.ChatRequest{
		BotID: row.BotID.String(), ThreadID: row.SessionID.String(), ChatID: data.ChatID,
		PersistedUserMessageID: request.MessageID, RequestStaysInContext: data.Query == "",
		RunID: admission.RunID, RunHandle: admission.Handle, TurnID: admission.TurnID, TurnPosition: &admission.TurnPosition,
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
		return c.plan.ChatSetup(ctx, req, spec)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		c.driver.Drive(runCtx, spec)
	}()

	c.logger.InfoContext(runCtx, "interrupted session resumed", slog.String("previous_run_id", row.RunID.String()), slog.String("run_id", admission.RunID), slog.String("session_id", req.ThreadID))
	return done, nil
}
