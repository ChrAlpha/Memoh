package application

import (
	"context"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/models"
)

func (s *Service) newDiscussStepCommitter(ctx context.Context, cmd turn.StartTurnCommand, h *discussHandle, cfg native.RunConfig, modelID string) (*agentStepCommitter, error) {
	if h.admission.RunID == "" {
		return nil, nil
	}
	position := h.admission.TurnPosition
	req := ChatRequest{
		RunID: h.id, RunHandle: h.admission.Handle,
		TurnID: h.admission.TurnID, TurnPosition: &position,
		BotID: cmd.BotID, ChatID: cmd.BotID, ThreadID: cmd.ThreadID,
		SourceChannelIdentityID: cmd.SourceChannelIdentityID, CurrentChannel: cmd.CurrentChannel,
		UserMessagePersisted: true, SessionType: cfg.SessionType,
		SkipMemoryExtraction: cmd.SkipMemoryExtraction,
	}
	committer := s.newAgentStepCommitter(ctx, req, resolvedContext{runConfig: cfg, model: models.GetResponse{ID: modelID}})
	if committer == nil {
		return nil, nil
	}
	for i := len(cmd.DiscussCurrentSources) - 1; i >= 0; i-- {
		source := cmd.DiscussCurrentSources[i]
		if !source.Current || source.ID == "" {
			continue
		}
		switch source.Kind {
		case "external":
			located, err := s.messageService.LocateByExternalIDBySession(ctx, cmd.ThreadID, source.ID, 0, 0)
			if err != nil {
				return nil, err
			}
			committer.req.PersistedUserMessageID = located.TargetID
		case "history":
			message, err := s.messageService.GetByIDBySession(ctx, cmd.ThreadID, source.ID)
			if err != nil {
				return nil, err
			}
			committer.req.PersistedUserMessageID = message.ID
		default:
			continue
		}
		committer.turnRequestMessageID = committer.req.PersistedUserMessageID
		break
	}
	return committer, nil
}
