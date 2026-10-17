package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db"
)

// persistExternalRound writes the one round an External Agent runtime produced.
// An external runtime reports a whole turn at once — there are no step commits —
// so this round is the run's only history write, and its failure metadata is the
// failure's code and args rather than a step's.
func (s *Service) persistExternalRound(
	ctx context.Context,
	req runreq.ChatRequest,
	runtimeType, projectPath string,
	result external.PromptResult,
	f *outcome.RunFailure,
	turnCompleted bool,
	contextLifecycle *contextfrag.LifecycleHolder,
	reasoningTiming []messagepkg.ReasoningTimingSegment,
) error {
	req.RuntimeType = runtimeType
	meta := map[string]any{
		"runtime_type": runtimeType,
		"project_path": projectPath,
		"stop_reason":  result.StopReason,
	}
	for key, value := range result.RoundMetadata {
		meta[key] = value
	}
	if f != nil {
		// The failure is recorded by its code and catalog args, which the
		// history view renders. The cause's text stays out of history.
		meta["agent_turn_outcome"] = "failed"
		meta = persist.FailureRowMetadata(*f, "", meta)
	}
	output := runreq.SDKMessagesToModelMessages(result.Output)
	if len(output) == 0 {
		output = []runreq.ModelMessage{{Role: "assistant", Content: runreq.NewTextContent("")}}
	}
	output = runreq.RepairToolCallClosures(output, runreq.SyntheticToolClosureError)
	if len(result.SteerInputIDs) > 0 && output[len(output)-1].Role != "assistant" {
		// A stop may arrive after the final steer input, before its reply.
		// Keep the terminal metadata and fork anchor after that input, even
		// when earlier portions of this runtime turn already had output.
		output = append(output, runreq.ModelMessage{Role: "assistant", Content: runreq.NewTextContent("")})
	}
	hasAssistant := false
	for _, msg := range output {
		if msg.Role == "assistant" {
			hasAssistant = true
			break
		}
	}
	if !hasAssistant {
		return errors.New("external transcript has no assistant message to publish")
	}
	if result.Usage != nil {
		for idx := len(output) - 1; idx >= 0; idx-- {
			if output[idx].Role == "assistant" {
				usage, _ := json.Marshal(result.Usage)
				output[idx].Usage = usage
				break
			}
		}
	}
	round := make([]runreq.ModelMessage, 0, 1+len(output))
	round = append(round, runreq.ModelMessage{Role: "user", Content: runreq.NewTextContent(req.Query)})
	round = append(round, output...)

	metadataByIndex := make(map[int]map[string]any, len(output))
	metadataOffset := 1
	if req.UserMessagePersisted || req.ReusePersistedUserMessage {
		metadataOffset = 0
	}
	firstAssistantIndex, lastAssistantIndex := -1, -1
	for idx, msg := range output {
		if msg.Role == "assistant" {
			if firstAssistantIndex < 0 {
				firstAssistantIndex = idx
			}
			lastAssistantIndex = idx
			entryMeta := make(map[string]any, len(meta))
			for key, value := range meta {
				entryMeta[key] = value
			}
			metadataByIndex[idx+metadataOffset] = entryMeta
		}
	}
	if f == nil && lastAssistantIndex >= 0 {
		ended := make(map[string]any, len(meta)+1)
		for key, value := range meta {
			ended[key] = value
		}
		// Aborted partial rounds commit as "aborted", not "succeeded": the
		// commit-unknown reconciliation needs the durable marker.
		if turnCompleted {
			ended["agent_turn_outcome"] = "succeeded"
		} else {
			ended["agent_turn_outcome"] = "aborted"
		}
		metadataByIndex[lastAssistantIndex+metadataOffset] = ended
	}
	agentTurnID := strings.TrimSpace(result.AgentTurnID)
	if (len(result.SteerInputIDs) > 0 || result.FinalTurnAnchorOnly) && lastAssistantIndex >= 0 {
		// Codex forks at whole runtime turns. Steer splits and Goal continuation
		// collect multiple assistant rows under one publication; only the tail
		// has a runtime anchor matching the complete visible history.
		metadataByIndex[lastAssistantIndex+metadataOffset]["agent_turn_id"] = agentTurnID
		agentTurnID = "" // Do not bulk-assign this anchor to earlier assistant rows.
	}
	// Keep each notice on one row only, including notice-only aborted rounds.
	// Row metadata survives history refresh/fork without entering model input.
	if firstAssistantIndex >= 0 && len(result.Notices) > 0 {
		metadataByIndex[firstAssistantIndex+metadataOffset][event.RuntimeNoticesMetadataKey] = result.Notices
	}
	// ACP uses the committed run ID to detect stale warm processes. Failed or
	// interrupted turns keep the previously observed head.
	var publication *messagepkg.AgentPublication
	if f == nil && turnCompleted && lastAssistantIndex >= 0 && result.PublishHead {
		publication = &messagepkg.AgentPublication{RunID: req.RunID}
	}
	skipMemory := f != nil || req.UserMessagePersisted || req.ReusePersistedUserMessage || req.SkipMemoryExtraction
	persisted, err := s.persist.StoreRoundResult(ctx, req, round, "", persist.RoundOptions{
		AllowPendingToolCalls:             true,
		SkipMemory:                        skipMemory,
		AllowEmptyAssistantText:           true,
		MessageMetadataByIndex:            metadataByIndex,
		ReasoningTiming:                   reasoningTiming,
		RequireCompletePersist:            true,
		CleanupRuntimeDecisionProjections: true,
		AgentPublication:                  publication,
		// The runtime's turn id lands on the run row in the round transaction;
		// it anchors turn-level operations such as codex thread/fork.
		AgentTurnID:      agentTurnID,
		ContextLifecycle: contextLifecycle,
	})
	if err == nil && runreq.LastPersistedAssistantMessageID(persisted) == "" {
		// Fires after a committed transaction: join the commit-unknown
		// sentinel so callers route into database reconciliation instead of
		// the definite-rollback compensation.
		err = errors.Join(db.ErrCommitOutcomeUnknown, errors.New("external assistant output was not persisted"))
	}
	if err == nil {
		s.publishRuntimeSteerHistory(ctx, req, result.SteerInputIDs, persisted)
	}
	if err == nil && f == nil && (req.UserMessagePersisted || req.ReusePersistedUserMessage) && !req.SkipMemoryExtraction {
		s.storeMemory(context.WithoutCancel(ctx), req, persisted)
	}
	return err
}
