package pending

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/internal/admit"
	"github.com/felinics/memoh/internal/agent/internal/drive"
	"github.com/felinics/memoh/internal/agent/internal/outcome"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/auth"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/httpx"
	"github.com/felinics/memoh/internal/logger"
)

// CoverStates are the run states that cover a discuss batch. A completed
// run has answered its batch, including by choosing to stay silent; an aborted,
// failed or lost run has not, so its batch stays pending and is answered with
// the next external input.
var CoverStates = []string{string(ledger.StateCompleted)}

// DiscussAdmission is what one CheckAndAdmitDiscuss call did.
type DiscussAdmission int

const (
	// DiscussAdmitted means a run now answers the pending external input.
	DiscussAdmitted DiscussAdmission = iota + 1
	// DiscussNothingPending means every external input is already covered.
	DiscussNothingPending
	// DiscussBusy means another run holds the session. That run's end checks
	// the session again.
	DiscussBusy
	// DiscussSkipped means the pending input is not answered now: the session
	// does not answer uncovered events, its batch was already admitted once, an
	// External Agent runtime was not addressed, or the workspace is not ready.
	// A session whose continuation or queued batch waits is also skipped.
	DiscussSkipped
)

func (a DiscussAdmission) String() string {
	switch a {
	case DiscussAdmitted:
		return "admitted"
	case DiscussNothingPending:
		return "nothing_pending"
	case DiscussBusy:
		return "busy"
	case DiscussSkipped:
		return "skipped"
	default:
		return "unknown"
	}
}

// CheckAndAdmitDiscuss answers the external input a discuss session has not
// covered yet. It is called when an external input is committed, when a run of
// the session ends, and by the periodic sweep; any caller may call it at any
// time and as often as it likes.
//
// A session's cover position is the highest history position a completed
// discuss run covered. The pending batch is every external user row past it,
// up to the newest one (hw). The batch is answered by one run whose invocation
// id is "discuss:<session>:<hw>". The invocation unique index refuses a second
// admission of an unchanged batch, so a batch whose run was stopped, failed or
// lost is answered again only once a newer input moves hw.
//
// The run builds its context from history like any other turn, starts at the
// hw row, speaks only through the send tool, and acts for the participant who
// wrote the hw row. It runs after this call returns.
//
// A session an External Agent runtime drives is answered only when the pending
// batch addresses the bot; otherwise no run is written and the batch stays
// pending.
//
// The check is the discuss half of the session's pending admission: an owed
// continuation and queued follow-ups are started first, and a session that has
// one holds its slot for that run, which is DiscussBusy here.
func (in *Inputs) CheckAndAdmitDiscuss(ctx context.Context, botID, sessionID string) (DiscussAdmission, error) {
	return in.admitPending(ctx, botID, sessionID)
}

// admitDiscuss is the discuss check itself, returning, for an admitted run, the
// channel that closes when the run has ended. It runs inside admitPending.
func (in *Inputs) admitDiscuss(ctx context.Context, botID, sessionID string) (DiscussAdmission, <-chan struct{}, error) {
	pgBotID, err := db.ParseUUID(botID)
	if err != nil {
		return 0, nil, err
	}
	pgSessionID, err := db.ParseUUID(sessionID)
	if err != nil {
		return 0, nil, err
	}
	sess, err := in.queries.GetDiscussSession(ctx, dbsqlc.GetDiscussSessionParams{SessionID: pgSessionID, BotID: pgBotID})
	if errors.Is(err, pgx.ErrNoRows) {
		return DiscussSkipped, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("read discuss session: %w", err)
	}
	cover, err := in.discussCover(ctx, pgSessionID)
	if err != nil {
		return 0, nil, err
	}
	inputs, err := in.pendingInputs(ctx, pgSessionID, cover)
	if err != nil {
		return 0, nil, err
	}
	if len(inputs) == 0 {
		return DiscussNothingPending, nil, nil
	}
	head := inputs[len(inputs)-1]
	externalRuntime := sess.RuntimeType == sessionpkg.RuntimeACPAgent || sessionpkg.IsDirectRuntimeType(sess.RuntimeType)
	if externalRuntime && !turn.IsPrivateConversationType(sess.ConversationType.String) {
		// An External Agent runtime replays the whole context on every run, so
		// it runs only when someone asked it to. The first addressed input
		// answers everything pending before it.
		addressed := slices.ContainsFunc(inputs, func(input dbsqlc.ListDiscussPendingInputsRow) bool {
			return input.MentionsMe || input.RepliesToMe
		})
		if !addressed {
			return DiscussSkipped, nil, nil
		}
	}

	if err := in.workspaceReady(ctx, botID, ""); err != nil {
		in.logger.InfoContext(ctx, "workspace not ready; discuss batch deferred",
			slog.String("session_id", sessionID), slog.Any("error", err))
		return DiscussSkipped, nil, nil
	}

	runtimeOwner := ""
	if externalRuntime && sess.RuntimeOwnerUserID.Valid {
		runtimeOwner = sess.RuntimeOwnerUserID.String()
	}
	req, err := in.discussRequest(ctx, botID, sessionID, pgBotID, sess, runtimeOwner, head, externalRuntime)
	if err != nil {
		return 0, nil, err
	}
	hw := head.TurnPosition.Int64
	payload, err := json.Marshal(map[string]any{
		"mode":       sessionmode.Discuss,
		"bot_id":     botID,
		"session_id": sessionID,
	})
	if err != nil {
		return 0, nil, err
	}

	endActiveTurn, err := in.activeTurns.Begin()
	if err != nil {
		return 0, nil, err
	}
	runCtx, admission, release, err := in.admit.Triggered(ctx, admit.Trigger{
		BotID: botID, ThreadID: sessionID, InvocationID: DiscussInvocationID(sessionID, hw),
		Mode: sessionmode.Discuss, DiscussCoverPosition: &hw,
		Submission: payload, StopCause: outcome.ErrRunStopped,
		JoinTurn: &ledger.Turn{ID: head.TurnID.String(), Position: hw},
	})
	if err != nil {
		endActiveTurn()
		switch {
		case errors.Is(err, sessionruntime.ErrSessionBusy):
			return DiscussBusy, nil, nil
		case errors.Is(err, sessionruntime.ErrInvocationConflict):
			return DiscussSkipped, nil, nil
		default:
			return 0, nil, err
		}
	}
	// The run answers a batch, not the request of whichever input, run end or
	// sweep tick led to this admission, so it and its result record carry a
	// request id of their own.
	runCtx = logger.ContextWithRequestID(runCtx, httpx.NewRequestID())
	// The cover position read above may be stale: a run for a newer batch can
	// have completed between that read and this admission. Holding the
	// session's slot, this read is current, and a batch it already covers ends
	// here instead of being answered twice.
	current, err := in.discussCover(runCtx, pgSessionID)
	discussRun := drive.AdmittedRun{Handle: admission.Handle, Mode: sessionmode.Discuss}
	if err != nil {
		in.driver.Finish(runCtx, discussRun, drive.FromOutcome(outcome.RunOutcome{Cause: err}.Resolve(runCtx)))
		release()
		endActiveTurn()
		return 0, nil, err
	}
	if current >= hw {
		in.driver.Finish(runCtx, discussRun, drive.FromOutcome(outcome.RunOutcome{Status: sessionruntime.RunStatusCompleted}))
		release()
		endActiveTurn()
		return DiscussNothingPending, nil, nil
	}

	req.RunID = admission.RunID
	req.RunHandle = admission.Handle
	req.TurnID = admission.TurnID
	req.TurnPosition = &admission.TurnPosition
	spec := drive.RunSpec{
		Run: drive.AdmittedRun{
			Handle:       admission.Handle,
			TurnID:       admission.TurnID,
			TurnPosition: &admission.TurnPosition,
			Mode:         sessionmode.Discuss,
		},
		Req:      req,
		Delivery: drive.NoDelivery{},
		// The external input is already in history: a failure writes its row
		// against it rather than handing anything back.
		Input: outcome.InputAccepted,
	}
	// The run builds its context from history like any other turn, and the text
	// the model is prompted with is that context. A runtime that owns its own
	// conversation context does not read history itself, so the entry renders
	// the context into the text the run is prompted with. The two facts travel
	// together: a prompt that carries the whole context is also a reason to
	// start the runtime fresh, because a resumed one would be shown the same
	// batch twice. The assembly runs inside the run, after admission, so a wake
	// that answers nothing pays for nothing, and the rendered text is what the
	// run stores for a crash resume.
	spec.Setup = func(setupCtx context.Context, req runreq.ChatRequest) (drive.RunPlan, error) {
		if !externalRuntime {
			batch, err := in.discussBatch(setupCtx, pgSessionID, current)
			if err != nil {
				return drive.RunPlan{}, err
			}
			req.DiscussBatchMessageIDs = batch
		}
		if externalRuntime {
			prompt, err := in.discussExternalPrompt(setupCtx, req)
			if err != nil {
				return drive.RunPlan{}, err
			}
			req.ModelQuery = prompt
			req.ForceFreshRuntime = true
		}
		return in.plan.ChatSetup(setupCtx, req, spec)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer endActiveTurn()
		// The driver writes the run's terminal state; release only ends the
		// admission.
		defer release()
		in.driver.Drive(runCtx, spec)
	}()
	return DiscussAdmitted, done, nil
}

// discussRequest is the request a discuss run executes: the session's route,
// the participant who wrote the hw row, and credentials issued for them. The
// input is the hw row itself, already in history.
func (in *Inputs) discussRequest(ctx context.Context, botID, sessionID string, pgBotID pgtype.UUID, sess dbsqlc.GetDiscussSessionRow, runtimeOwner string, head dbsqlc.ListDiscussPendingInputsRow, externalRuntime bool) (runreq.ChatRequest, error) {
	bot, err := in.queries.GetBotByID(ctx, pgBotID)
	if err != nil {
		return runreq.ChatRequest{}, fmt.Errorf("read discuss bot: %w", err)
	}
	senderID := ""
	boundUserID := ""
	if head.SenderChannelIdentityID.Valid {
		senderID = head.SenderChannelIdentityID.String()
		userIDs, err := in.queries.ListUserIDsByChannelIdentity(ctx, head.SenderChannelIdentityID)
		if err != nil {
			return runreq.ChatRequest{}, fmt.Errorf("read discuss participant account: %w", err)
		}
		// An identity bound to more than one account names nobody, as on the
		// inbound path.
		if len(userIDs) == 1 {
			boundUserID = userIDs[0].String()
		}
	}
	routeID := ""
	if sess.RouteID.Valid {
		routeID = sess.RouteID.String()
	}
	chatToken, sessionBearer, err := in.signer.IssueRoute(auth.RouteCredentials{
		BotID:              botID,
		ChatID:             botID,
		RouteID:            routeID,
		ReplyTarget:        sess.ReplyTarget.String,
		ChannelIdentityID:  senderID,
		UserID:             boundUserID,
		ExternalRuntime:    externalRuntime,
		RuntimeOwnerUserID: runtimeOwner,
		BotOwnerUserID:     bot.OwnerUserID.String(),
	})
	if err != nil {
		return runreq.ChatRequest{}, fmt.Errorf("issue discuss credentials: %w", err)
	}
	req := runreq.ChatRequest{
		BotID:                     botID,
		ThreadID:                  sessionID,
		ChatID:                    botID,
		RouteID:                   routeID,
		RequiredHistoryMessageID:  head.ID.String(),
		PersistedUserMessageID:    head.ID.String(),
		ReusePersistedUserMessage: true,
		UserID:                    boundUserID,
		SourceChannelIdentityID:   senderID,
		Token:                     sessionBearer,
		ChatToken:                 chatToken,
		CurrentChannel:            sess.ChannelType.String,
		ReplyTarget:               sess.ReplyTarget.String,
		ConversationType:          sess.ConversationType.String,
		SessionType:               sessionmode.Discuss,
	}
	if externalRuntime {
		req.ToolHTTPURL = external.ToolGatewayURLFromEnv(botID)
	}
	return req, nil
}

// pendingInputs reads the external input past cover, oldest first.
func (in *Inputs) pendingInputs(ctx context.Context, sessionID pgtype.UUID, cover int64) ([]dbsqlc.ListDiscussPendingInputsRow, error) {
	inputs, err := in.queries.ListDiscussPendingInputs(ctx, dbsqlc.ListDiscussPendingInputsParams{
		SessionID:     sessionID,
		CoverPosition: pgtype.Int8{Int64: cover, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("read discuss pending input: %w", err)
	}
	return inputs, nil
}

// discussBatch lists the history rows past cover that a discuss run answers.
func (in *Inputs) discussBatch(ctx context.Context, sessionID pgtype.UUID, cover int64) ([]string, error) {
	inputs, err := in.pendingInputs(ctx, sessionID, cover)
	if err != nil {
		return nil, err
	}
	batch := make([]string, len(inputs))
	for i, input := range inputs {
		batch[i] = input.ID.String()
	}
	return batch, nil
}

func (in *Inputs) discussCover(ctx context.Context, sessionID pgtype.UUID) (int64, error) {
	cover, err := in.queries.GetDiscussCoverPosition(ctx, dbsqlc.GetDiscussCoverPositionParams{
		SessionID: sessionID,
		States:    CoverStates,
	})
	if err != nil {
		return 0, fmt.Errorf("read discuss cover position: %w", err)
	}
	return cover, nil
}

// DiscussInvocationID identifies the run that answers a session's batch up to
// hw. It carries nothing else, so re-admitting an unchanged batch is the same
// invocation.
func DiscussInvocationID(sessionID string, hw int64) string {
	return "discuss:" + sessionID + ":" + strconv.FormatInt(hw, 10)
}

// AdmitDiscussRun is CheckAndAdmitDiscuss for a caller that follows the run it
// admitted: done closes when that run has ended, and is nil when none was
// admitted. Queued follow-ups are not started first.
func (in *Inputs) AdmitDiscussRun(ctx context.Context, botID, sessionID string) (DiscussAdmission, <-chan struct{}, error) {
	unlock := in.sessions.lock(botID + ":" + sessionID)
	defer unlock()
	return in.admitDiscuss(ctx, botID, sessionID)
}
