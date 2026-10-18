package application

import (
	"context"

	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/web"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// The Web client's sends, retries and edits are the Web runner's.

// AdmitWebRun admits a Web submission.
func (s *Service) AdmitWebRun(ctx context.Context, in web.AdmitInput) (*web.Admission, error) {
	return s.web.Admit(ctx, in)
}

// RunWebSend executes an admitted Web send.
func (s *Service) RunWebSend(admission *web.Admission, req runreq.ChatRequest, delivery drive.RunDelivery) {
	s.web.RunSend(admission, req, delivery)
}

// RunWebRetry executes an admitted retry of the session's latest turn.
func (s *Service) RunWebRetry(admission *web.Admission, input web.RetryLatestMessageInput, delivery drive.RunDelivery) {
	s.web.RunRetry(admission, input, delivery)
}

// RunWebEdit executes an admitted edit of the session's latest turn.
func (s *Service) RunWebEdit(admission *web.Admission, input web.EditLatestMessageInput, delivery drive.RunDelivery) {
	s.web.RunEdit(admission, input, delivery)
}

// PrepareRetryLatestTurnOperation validates the replacement target of a retry
// and returns the message where the old assistant tail begins.
func (s *Service) PrepareRetryLatestTurnOperation(ctx context.Context, sessionID, turnID string) (string, error) {
	return s.web.PrepareRetry(ctx, sessionID, turnID)
}

// PrepareEditLatestTurnOperation validates the replacement target of an edit
// and returns the user message where the old turn begins.
func (s *Service) PrepareEditLatestTurnOperation(ctx context.Context, sessionID, turnID string) (string, error) {
	return s.web.PrepareEdit(ctx, sessionID, turnID)
}

// ResolveTurnIDForMessage maps a stored message id onto the round that contains it.
func (s *Service) ResolveTurnIDForMessage(ctx context.Context, sessionID, messageID string) (string, error) {
	return s.web.ResolveTurnID(ctx, sessionID, messageID)
}

// ApplyUserMessageHookAndPersistUserTurn applies the user-message hook before
// writing a user turn ahead of agent execution.
func (s *Service) ApplyUserMessageHookAndPersistUserTurn(ctx context.Context, req runreq.ChatRequest) (runreq.ChatRequest, messagepkg.Message, error) {
	return s.web.PersistUserTurn(ctx, req)
}
