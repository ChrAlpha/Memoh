package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/runtime/external"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/auth"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// discussCoverStates are the run states that cover a discuss batch. A completed
// run has answered its batch, including by choosing to stay silent; an aborted,
// failed or lost run has not, so its batch stays pending and is answered with
// the next external input.
var discussCoverStates = []string{string(ledger.StateCompleted)}

// discussCredentialTTL is the lifetime of the credentials a discuss admission
// signs. It is the inbound path's, so a discuss run holds the same credentials
// however it was started.
const discussCredentialTTL = 5 * time.Minute

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
	// does not answer uncovered events, its batch was already admitted once, or
	// an External Agent runtime was not addressed.
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
// The check is the discuss half of the session's pending admission: queued
// follow-ups are started first, and a session that has one holds its slot for
// that run, which is DiscussBusy here.
func (s *Service) CheckAndAdmitDiscuss(ctx context.Context, botID, sessionID string) (DiscussAdmission, error) {
	return s.admitPending(ctx, botID, sessionID)
}

// admitDiscuss is the discuss check itself, returning, for an admitted run, the
// channel that closes when the run has ended. It runs inside admitPending.
func (s *Service) admitDiscuss(ctx context.Context, botID, sessionID string) (DiscussAdmission, <-chan struct{}, error) {
	if s.queries == nil || s.sessionRuntime == nil {
		return 0, nil, errors.New("discuss admission is not configured")
	}
	pgBotID, err := db.ParseUUID(botID)
	if err != nil {
		return 0, nil, err
	}
	pgSessionID, err := db.ParseUUID(sessionID)
	if err != nil {
		return 0, nil, err
	}
	sess, err := s.queries.GetDiscussSession(ctx, dbsqlc.GetDiscussSessionParams{SessionID: pgSessionID, BotID: pgBotID})
	if errors.Is(err, pgx.ErrNoRows) {
		return DiscussSkipped, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("read discuss session: %w", err)
	}
	cover, err := s.discussCover(ctx, pgSessionID)
	if err != nil {
		return 0, nil, err
	}
	head, err := s.queries.GetDiscussPendingHead(ctx, dbsqlc.GetDiscussPendingHeadParams{
		SessionID:     pgSessionID,
		CoverPosition: pgtype.Int8{Int64: cover, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return DiscussNothingPending, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("read discuss pending input: %w", err)
	}
	externalRuntime := sess.RuntimeType == sessionpkg.RuntimeACPAgent || sessionpkg.IsDirectRuntimeType(sess.RuntimeType)
	if externalRuntime && !turn.IsPrivateConversationType(sess.ConversationType.String) {
		// An External Agent runtime replays the whole context on every run, so
		// it runs only when someone asked it to. The first addressed input
		// answers everything pending before it.
		addressed, err := s.queries.GetDiscussPendingAddressed(ctx, dbsqlc.GetDiscussPendingAddressedParams{
			SessionID:     pgSessionID,
			CoverPosition: pgtype.Int8{Int64: cover, Valid: true},
		})
		if err != nil {
			return 0, nil, fmt.Errorf("read discuss addressed input: %w", err)
		}
		if !addressed {
			return DiscussSkipped, nil, nil
		}
	}

	req, err := s.discussRequest(ctx, botID, sessionID, pgBotID, sess, head, externalRuntime)
	if err != nil {
		return 0, nil, err
	}
	hw := head.TurnPosition.Int64
	payload, err := json.Marshal(map[string]any{
		"mode":           sessionmode.Discuss,
		"bot_id":         botID,
		"session_id":     sessionID,
		"cover_position": hw,
	})
	if err != nil {
		return 0, nil, err
	}

	endActiveTurn, err := s.activeTurns.begin()
	if err != nil {
		return 0, nil, err
	}
	runCtx, admission, finish, release, err := s.admitTriggeredRun(ctx, sessionmode.Discuss, botID, sessionID, discussInvocationID(sessionID, hw), payload, nil, errRunStopped)
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
	// The cover position read above may be stale: a run for a newer batch can
	// have completed between that read and this admission. Holding the
	// session's slot, this read is current, and a batch it already covers ends
	// here instead of being answered twice.
	current, err := s.discussCover(runCtx, pgSessionID)
	if err != nil {
		finish(RunOutcome{Cause: err})
		endActiveTurn()
		return 0, nil, err
	}
	if current >= hw {
		finish(RunOutcome{Status: sessionruntime.RunStatusCompleted})
		endActiveTurn()
		return DiscussNothingPending, nil, nil
	}

	req.RunID = admission.RunID
	req.RunHandle = admission.Handle
	req.TurnID = admission.TurnID
	req.TurnPosition = &admission.TurnPosition
	spec := runSpec{
		Run: admittedRun{
			Handle:       admission.Handle,
			TurnID:       admission.TurnID,
			TurnPosition: &admission.TurnPosition,
			Mode:         sessionmode.Discuss,
		},
		Req:      req,
		Delivery: noDelivery{},
		// The external input is already in history: a failure writes its row
		// against it rather than handing anything back.
		Input: inputAccepted,
	}
	spec.Setup = func(setupCtx context.Context, req ChatRequest) (runPlan, error) {
		return s.chatSetup(setupCtx, req, spec)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer endActiveTurn()
		// The driver writes the run's terminal state; release only ends the
		// admission.
		defer release()
		s.drive(runCtx, spec)
	}()
	return DiscussAdmitted, done, nil
}

// discussRequest is the request a discuss run executes: the session's route,
// the participant who wrote the hw row, and credentials issued for them. The
// input is the hw row itself, already in history.
func (s *Service) discussRequest(ctx context.Context, botID, sessionID string, pgBotID pgtype.UUID, sess dbsqlc.GetDiscussSessionRow, head dbsqlc.GetDiscussPendingHeadRow, externalRuntime bool) (ChatRequest, error) {
	bot, err := s.queries.GetBotByID(ctx, pgBotID)
	if err != nil {
		return ChatRequest{}, fmt.Errorf("read discuss bot: %w", err)
	}
	senderID := ""
	boundUserID := ""
	if head.SenderChannelIdentityID.Valid {
		senderID = head.SenderChannelIdentityID.String()
		userIDs, err := s.queries.ListUserIDsByChannelIdentity(ctx, head.SenderChannelIdentityID)
		if err != nil {
			return ChatRequest{}, fmt.Errorf("read discuss participant account: %w", err)
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
	chatToken, sessionBearer, err := auth.IssueRouteCredentials(auth.RouteCredentials{
		BotID:              botID,
		ChatID:             botID,
		RouteID:            routeID,
		ReplyTarget:        sess.ReplyTarget.String,
		ChannelIdentityID:  senderID,
		UserID:             boundUserID,
		ExternalRuntime:    externalRuntime,
		RuntimeOwnerUserID: sess.RuntimeOwnerAccountID,
		BotOwnerUserID:     bot.OwnerUserID.String(),
	}, s.resumeSecret, discussCredentialTTL)
	if err != nil {
		return ChatRequest{}, fmt.Errorf("issue discuss credentials: %w", err)
	}
	req := ChatRequest{
		BotID:                     botID,
		ThreadID:                  sessionID,
		ChatID:                    botID,
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

func (s *Service) discussCover(ctx context.Context, sessionID pgtype.UUID) (int64, error) {
	cover, err := s.queries.GetDiscussCoverPosition(ctx, dbsqlc.GetDiscussCoverPositionParams{
		SessionID: sessionID,
		States:    discussCoverStates,
	})
	if err != nil {
		return 0, fmt.Errorf("read discuss cover position: %w", err)
	}
	return cover, nil
}

// discussInvocationID identifies the run that answers a session's batch up to
// hw. It carries nothing else, so re-admitting an unchanged batch is the same
// invocation.
func discussInvocationID(sessionID string, hw int64) string {
	return "discuss:" + sessionID + ":" + strconv.FormatInt(hw, 10)
}
