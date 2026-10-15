package application

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

func (c *agentStepCommitter) bindContextSwitch(cfg *native.RunConfig) {
	if c == nil || cfg == nil || c.service == nil || c.service.compactionService == nil || c.service.settingsService == nil || c.service.queries == nil {
		return
	}
	modelID := cfg.CurrentModelUUID
	cfg.SwitchContext = func(ctx context.Context, input native.ContextSwitchInput) (native.ContextSwitchResult, error) {
		if err := c.contextSwitchOwnership(ctx, input.RunID); err != nil {
			return native.ContextSwitchResult{}, err
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		result, err := c.service.prepareContextSwitch(ctx, c.req, modelID, input)
		if ownerErr := c.contextSwitchOwnership(ctx, input.RunID); ownerErr != nil {
			return native.ContextSwitchResult{}, ownerErr
		}
		return result, err
	}
}

func (c *agentStepCommitter) contextSwitchOwnership(ctx context.Context, runID string) error {
	if c.req.RunID != runID {
		return fmt.Errorf("%w: context switch run identity changed", contextfrag.ErrBudgetUnsatisfied)
	}
	if c.ownerContext != nil && c.ownerContext.Err() != nil {
		return context.Cause(c.ownerContext)
	}
	return ctx.Err()
}

func (s *Service) prepareContextSwitch(ctx context.Context, req ChatRequest, modelID string, input native.ContextSwitchInput) (native.ContextSwitchResult, error) {
	if len(input.Sources) != len(input.Request.Messages) || len(input.Protected) != len(input.Sources) || input.InputAllowanceTokens <= 0 {
		return native.ContextSwitchResult{}, fmt.Errorf("%w: invalid context switch basis", contextfrag.ErrBudgetUnsatisfied)
	}
	loadCandidate := func() (native.ContextSwitchResult, error) {
		frontier, err := s.loadActiveCompactionFrontier(ctx, req.BotID, req.ThreadID)
		if err != nil {
			return native.ContextSwitchResult{}, err
		}
		return projectContextSwitch(input, frontier.Artifacts), nil
	}
	candidate, err := loadCandidate()
	if err != nil {
		return native.ContextSwitchResult{}, err
	}
	pressure := contextfrag.ProviderEnvelopeTokens(input.Request.System, input.Request.Messages, input.Request.Tools)
	fits := func(candidate native.ContextSwitchResult) bool {
		cost := contextfrag.ProviderEnvelopeTokens(input.Request.System, candidate.Messages, input.Request.Tools)
		return candidate.Messages != nil && cost < pressure && cost <= input.InputAllowanceTokens
	}
	if fits(candidate) {
		return candidate, nil
	}
	botSettings, err := s.settingsService.GetBot(ctx, req.BotID)
	if err != nil {
		return native.ContextSwitchResult{}, err
	}
	if !botSettings.CompactionEnabled {
		return native.ContextSwitchResult{}, nil
	}
	cfg, err := s.buildCompactionConfig(ctx, req, botSettings, pressure, modelID)
	if err != nil || cfg.ModelID == "" {
		return native.ContextSwitchResult{}, err
	}
	cfg.AllowFrontierFusion = true
	cfg.HardPressure = true
	cfg.ContextWindowTokens = input.InputAllowanceTokens
	for range 3 {
		current := input
		if candidate.Messages != nil && contextfrag.ProviderEnvelopeTokens(input.Request.System, candidate.Messages, input.Request.Tools) < pressure {
			current.Request.Messages, current.Sources = candidate.Messages, candidate.Sources
			current.Protected = make([]bool, len(candidate.Messages))
			for i, source := range candidate.SourceIndexes {
				current.Protected[i] = source >= 0 && input.Protected[source]
			}
		}
		eligible, budget, scopeErr := contextSwitchCompactionScope(current)
		if scopeErr != nil || len(eligible) == 0 {
			return native.ContextSwitchResult{}, scopeErr
		}
		cfg.EligibleSources = eligible
		cfg.HistoryBudgetTokens = budget
		cfg.TargetTokens = compactionTargetTokens(botSettings.CompactionTargetPercent, budget)
		previous := candidate
		result, compactErr := s.compactionService.RunCompactionSync(ctx, cfg)
		if ctx.Err() != nil {
			return native.ContextSwitchResult{}, ctx.Err()
		}
		if compactErr != nil {
			return native.ContextSwitchResult{}, fmt.Errorf("%w: compaction candidate: %w", contextfrag.ErrBudgetUnsatisfied, compactErr)
		}
		candidate, err = loadCandidate()
		if err != nil {
			return native.ContextSwitchResult{}, err
		}
		if fits(candidate) {
			return candidate, nil
		}
		if result.Status != compaction.StatusOK && result.Status != compaction.StatusProgress || reflect.DeepEqual(candidate.Messages, previous.Messages) {
			return native.ContextSwitchResult{}, nil
		}
	}
	return native.ContextSwitchResult{}, nil
}
