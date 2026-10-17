package application

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runreq"
	turnpkg "github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// RetryLatestMessageInput names the turn a Web retry replaces and who asks.
// The run that replaces it is the admission's, so the input carries none of
// the run's identity.
type RetryLatestMessageInput struct {
	BotID     string
	SessionID string
	// TargetTurnID names the round being replaced. It is distinct from the
	// admission's turn, which names the new turn this operation runs.
	TargetTurnID           string
	ActorChannelIdentityID string
	ActorUserID            string
	ChatToken              string
	Model                  string
	ReasoningEffort        string
	WorkspaceTargetID      string
	ToolHTTPURL            string
	// OnModelPreferenceSettled releases subsequent picker writes once this
	// turn's preference write-back has finished (issue #879). Same contract
	// as ChatRequest.OnModelPreferenceSettled.
	OnModelPreferenceSettled func()
}

// EditLatestMessageInput names the turn a Web edit replaces and its new text.
type EditLatestMessageInput struct {
	BotID     string
	SessionID string
	// TargetTurnID names the round being replaced. See RetryLatestMessageInput.
	TargetTurnID           string
	Text                   string
	Attachments            []runreq.ChatAttachment
	ActorChannelIdentityID string
	ActorUserID            string
	ChatToken              string
	Model                  string
	ReasoningEffort        string
	WorkspaceTargetID      string
	ToolHTTPURL            string
	// OnModelPreferenceSettled: see RetryLatestMessageInput.
	OnModelPreferenceSettled func()
}

// retryRunSpec is the run of a Web retry. The turn it replaces is resolved in
// the run's setup: a target that is no longer the latest turn is the run's own
// failure, reported to the caller like any other.
func (s *Service) retryRunSpec(admission *WebAdmission, input RetryLatestMessageInput, delivery drive.RunDelivery) drive.RunSpec {
	sessionID := strings.TrimSpace(input.SessionID)
	targetTurnID := strings.TrimSpace(input.TargetTurnID)
	replace := &webReplacement{service: s, reason: "retry", oldTurnID: targetTurnID}
	spec := s.webRunSpec(admission, runreq.ChatRequest{
		BotID:                    strings.TrimSpace(input.BotID),
		ChatID:                   strings.TrimSpace(input.BotID),
		ThreadID:                 sessionID,
		UserID:                   strings.TrimSpace(input.ActorUserID),
		SourceChannelIdentityID:  strings.TrimSpace(input.ActorChannelIdentityID),
		ConversationType:         turnpkg.ConversationTypePrivate,
		Token:                    strings.TrimSpace(input.ChatToken),
		ChatToken:                strings.TrimSpace(input.ChatToken),
		CurrentChannel:           "local",
		ReplyTarget:              strings.TrimSpace(input.BotID),
		Channels:                 []string{"local"},
		Model:                    strings.TrimSpace(input.Model),
		ReasoningEffort:          strings.TrimSpace(input.ReasoningEffort),
		WorkspaceTargetID:        strings.TrimSpace(input.WorkspaceTargetID),
		ToolHTTPURL:              strings.TrimSpace(input.ToolHTTPURL),
		SkipHistoryTurn:          true,
		OnModelPreferenceSettled: input.OnModelPreferenceSettled,
	}, delivery, outcome.InputAccepted)
	spec.Replacement = replace.hooks()
	spec.Setup = func(ctx context.Context, req runreq.ChatRequest) (drive.RunPlan, error) {
		turn, cutoffMessageID, err := s.prepareRetryLatestTurnOperation(ctx, sessionID, targetTurnID)
		if err != nil {
			return drive.RunPlan{}, err
		}
		requestMessageID := strings.TrimSpace(turn.RequestMessageID)
		requestMessage, err := s.messageService.GetByIDBySession(ctx, sessionID, requestMessageID)
		if err != nil {
			return drive.RunPlan{}, err
		}
		if !strings.EqualFold(requestMessage.Role, "user") {
			return drive.RunPlan{}, errors.New("retry target request is not a user message")
		}
		assistantMessage, err := s.messageService.GetByIDBySession(ctx, sessionID, strings.TrimSpace(turn.AssistantMessageID))
		if err != nil {
			return drive.RunPlan{}, err
		}
		// A turn the user-message hook refused never reached the model, so its
		// retry runs the hook again instead of reusing the refused message.
		retryUserMessageHook := isPreflightUserMessageHookFailure(assistantMessage)
		req.Query = persist.VisibleMessageText(requestMessage)
		req.RawQuery = persist.VisibleMessageText(requestMessage)
		req.ReusePersistedUserMessage = !retryUserMessageHook
		if !retryUserMessageHook {
			req.PersistedUserMessageID = requestMessage.ID
		}
		req.HistoryCutoffBeforeMessageID = cutoffMessageID
		req.RequiredHistoryMessageID = requestMessage.ID
		return s.chatSetup(ctx, replace.target(ctx, req, requestMessage.ID), spec)
	}
	return spec
}

func isPreflightUserMessageHookFailure(message messagepkg.Message) bool {
	return message.Metadata != nil &&
		message.Metadata[messagepkg.HistoryFailureOriginMetadataKey] == messagepkg.HistoryFailureOriginUserMessageHook
}

// editRunSpec is the run of a Web edit: the latest turn's request replaced by
// new text, and the turn answered again.
func (s *Service) editRunSpec(admission *WebAdmission, input EditLatestMessageInput, delivery drive.RunDelivery) drive.RunSpec {
	sessionID := strings.TrimSpace(input.SessionID)
	targetTurnID := strings.TrimSpace(input.TargetTurnID)
	text := strings.TrimSpace(input.Text)
	replace := &webReplacement{service: s, reason: "edit", oldTurnID: targetTurnID}
	spec := s.webRunSpec(admission, runreq.ChatRequest{
		BotID:                    strings.TrimSpace(input.BotID),
		ChatID:                   strings.TrimSpace(input.BotID),
		ThreadID:                 sessionID,
		UserID:                   strings.TrimSpace(input.ActorUserID),
		SourceChannelIdentityID:  strings.TrimSpace(input.ActorChannelIdentityID),
		ConversationType:         turnpkg.ConversationTypePrivate,
		Query:                    text,
		RawQuery:                 text,
		Token:                    strings.TrimSpace(input.ChatToken),
		ChatToken:                strings.TrimSpace(input.ChatToken),
		CurrentChannel:           "local",
		ReplyTarget:              strings.TrimSpace(input.BotID),
		Channels:                 []string{"local"},
		Attachments:              input.Attachments,
		Model:                    strings.TrimSpace(input.Model),
		ReasoningEffort:          strings.TrimSpace(input.ReasoningEffort),
		WorkspaceTargetID:        strings.TrimSpace(input.WorkspaceTargetID),
		ToolHTTPURL:              strings.TrimSpace(input.ToolHTTPURL),
		SkipHistoryTurn:          true,
		OnModelPreferenceSettled: input.OnModelPreferenceSettled,
	}, delivery, outcome.InputAccepted)
	spec.Replacement = replace.hooks()
	spec.Setup = func(ctx context.Context, req runreq.ChatRequest) (drive.RunPlan, error) {
		if text == "" && len(input.Attachments) == 0 {
			return drive.RunPlan{}, errors.New("message text or attachments required")
		}
		turn, err := s.prepareEditLatestTurnOperation(ctx, sessionID, targetTurnID)
		if err != nil {
			return drive.RunPlan{}, err
		}
		req.HistoryCutoffBeforeMessageID = strings.TrimSpace(turn.RequestMessageID)
		return s.chatSetup(ctx, replace.target(ctx, req, ""), spec)
	}
	return spec
}

// webReplacement is the turn a retry or an edit replaces. Its request is
// resolved in the run's setup, and the replacement hooks read it from there:
// the preflight checks the turn is still the latest once the workspace is
// ready, and the history write rewrites it once the new turn is durable.
type webReplacement struct {
	service   *Service
	reason    string
	oldTurnID string

	req              runreq.ChatRequest
	requestMessageID string
}

func (r *webReplacement) hooks() *drive.TurnReplacement {
	return &drive.TurnReplacement{
		Preflight: func(ctx context.Context) error {
			return r.service.ensureLatestVisibleTurn(ctx, r.req.ThreadID, r.oldTurnID)
		},
		PostPersist: func(ctx context.Context, persisted []messagepkg.Message) error {
			return r.service.persist.ReplaceTurn(ctx, r.req, r.oldTurnID, r.requestMessageID, r.reason, persisted)
		},
	}
}

// target records the request that replaces the turn and returns it carrying
// the replacement the history write stores the new turn under.
func (r *webReplacement) target(ctx context.Context, req runreq.ChatRequest, requestMessageID string) runreq.ChatRequest {
	replacement := &messagepkg.TurnReplacement{
		OldTurnID:               r.oldTurnID,
		ReplacementTurnID:       strings.TrimSpace(req.TurnID),
		ReplacementTurnPosition: req.TurnPosition,
		RequestMessageID:        strings.TrimSpace(requestMessageID),
		Reason:                  r.reason,
	}
	if update := r.service.persist.PrepareForkAnchorUpdate(ctx, req.ThreadID, req.HistoryCutoffBeforeMessageID); update != nil {
		replacement.SessionMetadata = update.Metadata
	}
	req.TurnReplacement = replacement
	r.req = req
	r.requestMessageID = requestMessageID
	return req
}

// PrepareRetryLatestTurnOperation validates the replacement target before
// Session Runtime publishes it to subscribers and returns the exact persisted
// message where the old assistant tail begins.
func (s *Service) PrepareRetryLatestTurnOperation(ctx context.Context, sessionID, turnID string) (string, error) {
	_, replaceFromMessageID, err := s.prepareRetryLatestTurnOperation(ctx, strings.TrimSpace(sessionID), strings.TrimSpace(turnID))
	return replaceFromMessageID, err
}

func (s *Service) prepareRetryLatestTurnOperation(ctx context.Context, sessionID, turnID string) (messagepkg.HistoryTurn, string, error) {
	turn, err := s.resolveLatestVisibleTurn(ctx, sessionID, turnID, "only the latest assistant message can be retried")
	if err != nil {
		return messagepkg.HistoryTurn{}, "", err
	}
	if strings.TrimSpace(turn.RequestMessageID) == "" {
		return messagepkg.HistoryTurn{}, "", errors.New("retry target has no request message")
	}
	// The turn's own assistant anchor, not a message the client named: a turn
	// that reached the client without one is a history the server cannot cut.
	// The request message is still there, so the client may edit instead.
	replaceFromMessageID := strings.TrimSpace(turn.AssistantMessageID)
	if replaceFromMessageID == "" {
		return messagepkg.HistoryTurn{}, "", apperror.Wrap(
			apperror.CodeSessionTurnIncomplete,
			fmt.Errorf("turn %s has no assistant anchor", turn.ID),
			nil,
		)
	}
	return turn, replaceFromMessageID, nil
}

// PrepareEditLatestTurnOperation validates the replacement target before
// Session Runtime publishes it to subscribers and returns the exact persisted
// user message where the old turn begins.
func (s *Service) PrepareEditLatestTurnOperation(ctx context.Context, sessionID, turnID string) (string, error) {
	turn, err := s.prepareEditLatestTurnOperation(ctx, strings.TrimSpace(sessionID), strings.TrimSpace(turnID))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(turn.RequestMessageID), nil
}

func (s *Service) prepareEditLatestTurnOperation(ctx context.Context, sessionID, turnID string) (messagepkg.HistoryTurn, error) {
	turn, err := s.resolveLatestVisibleTurn(ctx, sessionID, turnID, "only the latest user message can be edited")
	if err != nil {
		return messagepkg.HistoryTurn{}, err
	}
	// The turn's request message is by construction its leading user message,
	// so naming the turn already names what an edit replaces.
	if strings.TrimSpace(turn.RequestMessageID) == "" {
		return messagepkg.HistoryTurn{}, errors.New("edit target has no request message")
	}
	return turn, nil
}

// ResolveTurnIDForMessage maps a stored message id onto the round that contains
// it. It backs the compatibility shim for the pre-turn `message_id` spelling
// and has no other caller: a client holds a turn id from admission onward and
// names the round directly, while needing a stored message id is exactly what
// used to force it to wait for the round to persist.
//
// Remove it, and the shim, once the compatibility window for `message_id`
// closes. (Not marked Deprecated in the godoc sense — the shim is its intended
// caller, so flagging that call adds noise rather than signal.)
func (s *Service) ResolveTurnIDForMessage(ctx context.Context, sessionID, messageID string) (string, error) {
	if s == nil || s.messageService == nil {
		return "", errors.New("message service not configured")
	}
	sessionID = strings.TrimSpace(sessionID)
	messageID = strings.TrimSpace(messageID)
	if sessionID == "" {
		return "", errors.New("session id is required")
	}
	if messageID == "" {
		return "", errors.New("message id is required")
	}
	turn, err := s.messageService.GetVisibleTurnByMessage(ctx, sessionID, messageID)
	if err != nil {
		return "", fmt.Errorf("load visible turn: %w", err)
	}
	turnID := strings.TrimSpace(turn.ID)
	if turnID == "" {
		return "", errors.New("message has no visible turn")
	}
	return turnID, nil
}

// resolveLatestVisibleTurn answers which round the client named, from the turn
// alone. Retry and edit can only ever address the moving tail, so "is this the
// latest visible turn" is the entire validation; the turn then carries its own
// canonical request/assistant message ids, which is why no message lookup is
// needed to find the round.
//
// The turn is also the only identity both sides hold at the same time. A turn
// id is minted at admission and reaches the client while the run is still
// streaming, whereas a database message id exists only once the round is
// persisted — keying on the turn is what lets a live round be retried or
// edited without first waiting for its stored twin to arrive.
func (s *Service) resolveLatestVisibleTurn(ctx context.Context, sessionID, turnID, notLatest string) (messagepkg.HistoryTurn, error) {
	if s == nil || s.messageService == nil {
		return messagepkg.HistoryTurn{}, errors.New("message service not configured")
	}
	if sessionID == "" {
		return messagepkg.HistoryTurn{}, errors.New("session id is required")
	}
	if turnID == "" {
		return messagepkg.HistoryTurn{}, errors.New("turn id is required")
	}
	latest, err := s.messageService.GetLatestVisibleTurnBySession(ctx, sessionID)
	if err != nil {
		// A session with no visible turn at all and a session whose latest turn
		// is a different one are the same answer to the client: the round it
		// named is not the one the server can replace, so it has to reload.
		// Both are stale-client conflicts, not history faults, and are reported
		// with the 409 code instead of the store's own wording.
		if errors.Is(err, messagepkg.ErrNoVisibleTurn) {
			return messagepkg.HistoryTurn{}, apperror.Wrap(apperror.CodeSessionTurnNotLatest, err, nil)
		}
		return messagepkg.HistoryTurn{}, fmt.Errorf("load latest visible turn: %w", err)
	}
	if strings.TrimSpace(latest.ID) != turnID {
		return messagepkg.HistoryTurn{}, apperror.Wrap(apperror.CodeSessionTurnNotLatest, errors.New(notLatest), nil)
	}
	return latest, nil
}

func (s *Service) ensureLatestVisibleTurn(ctx context.Context, sessionID, turnID string) error {
	_, err := s.resolveLatestVisibleTurn(ctx, sessionID, strings.TrimSpace(turnID), "turn is not latest")
	return err
}
