package application

import (
	"context"
	"log/slog"

	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
)

func (s *Service) prepareExternalDiscussContext(ctx context.Context, req ChatRequest, markdown string, imageCount int) (ChatRequest, error) {
	if req.discussMessages == nil {
		return req, nil
	}
	if err := ctx.Err(); err != nil {
		return req, err
	}
	admitted, admission := admitDiscussAgentContext(req.discussMessages, s.contextAbsoluteMaxTokens(), len(markdown)+len(req.discussPromptPrefix)+len(req.discussPromptSuffix), imageCount)
	if admission.ProtectedOverflow || admission.DroppedMessages > 0 || len(req.discussOmittedSources) > 0 || max(0, req.discussContextTokens-admission.ProtectedTokens) > admission.HistoryBudgetTokens {
		s.scheduleAllowanceCompaction(ctx, req, max(req.discussContextTokens, admission.EstimatedTokens), admission.HistoryBudgetTokens, "")
	}
	if admission.ProtectedOverflow {
		return req, apperror.New(apperror.CodeContextProtectedOverflow, nil)
	}
	if admission.DroppedMessages > 0 && s.logger != nil {
		s.logger.InfoContext(ctx, "context_admission",
			slog.String("path", "discuss_external_context"),
			slog.Int("selected_tokens", admission.SelectedTokens),
			slog.Int("budget_tokens", admission.BudgetTokens),
			slog.Int("dropped_messages", admission.DroppedMessages))
	}
	req.Query = req.discussPromptPrefix + discussAgentFullContextPrompt(admitted) + req.discussPromptSuffix
	req.RawQuery = req.Query
	req.discussMessages = admitted
	req.discussOmittedSources = append(append([]turn.ContextMessageSource(nil), req.discussOmittedSources...), admission.OmittedSources...)
	return req, nil
}
