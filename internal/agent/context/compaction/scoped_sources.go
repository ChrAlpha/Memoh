package compaction

import (
	"context"
	"slices"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

type compactionSourceScope map[string]contextfrag.ContextRef

func (s compactionSourceScope) messageIDs() []pgtype.UUID {
	if s == nil {
		return nil
	}
	ids := make([]pgtype.UUID, 0, len(s))
	for _, ref := range s {
		if ref.Namespace != "bot_history_message" {
			continue
		}
		id, err := db.ParseUUID(ref.ID)
		if err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func scopedCandidateBarriers(messages []CompactionCandidate, rows []sqlc.ListUncompactedMessagesBySessionWithinBytesRow) []CompactionCandidate {
	result := make([]CompactionCandidate, 0, len(messages))
	for i, message := range messages {
		if i > 0 && (rows[i-1].HistoryOrdinal <= 0 || rows[i].HistoryOrdinal != rows[i-1].HistoryOrdinal+1) {
			result = append(result, CompactionCandidate{Policies: []CompactPolicy{CompactPolicyMustKeep}})
		}
		result = append(result, message)
	}
	return result
}

func newCompactionSourceScope(refs []contextfrag.ContextRef) compactionSourceScope {
	if refs == nil {
		return nil
	}
	scope := make(compactionSourceScope, len(refs))
	for _, ref := range refs {
		if validatePersistedCoverageRef(ref) == nil {
			scope[ref.StableKey()] = ref
		}
	}
	return scope
}

func (s compactionSourceScope) includes(ref contextfrag.ContextRef) bool {
	expected, ok := s[ref.StableKey()]
	return ok && compatibleCoverageRef(ref, expected)
}

func (s compactionSourceScope) permitsClaimed(messages []CompactionCandidate, ids []pgtype.UUID) bool {
	if s == nil {
		return true
	}
	selected := make(map[pgtype.UUID]bool, len(ids))
	for _, id := range ids {
		selected[id] = true
	}
	for _, message := range messages {
		if selected[message.ID] && !s.includes(message.Record.Ref) {
			return false
		}
	}
	return true
}

func (s compactionSourceScope) permitsFusion(artifacts []Artifact, protected []turn.ContextMessageSource) bool {
	if s == nil {
		return true
	}
	for _, artifact := range artifacts {
		if len(artifact.Coverage) == 0 {
			return false
		}
		for _, covered := range artifact.Coverage {
			if !s.includes(covered.Ref) {
				return false
			}
			for _, source := range protected {
				if source.ID != "" && ((source.Kind == "history" && source.ID == covered.Ref.ID) ||
					(source.Kind == "external" && source.ID == covered.ExternalMessageID)) {
					return false
				}
			}
		}
	}
	return true
}

func (s *Service) loadScopedCandidateAssets(ctx context.Context, scope compactionSourceScope, messages []CompactionCandidate, rows []sqlc.ListUncompactedMessagesBySessionRow) ([]CompactionCandidate, error) {
	ids := make([]pgtype.UUID, 0, len(messages))
	for _, message := range messages {
		if _, ok := scope[message.Record.Ref.StableKey()]; ok {
			ids = append(ids, message.ID)
		}
	}
	if len(ids) == 0 {
		return messages, nil
	}
	assets, err := s.queries.ListMessageAssetsBatch(ctx, ids)
	if err != nil {
		return nil, err
	}
	return candidatesWithAssets(messages, rows, assets)
}

func (s compactionSourceScope) protect(messages []CompactionCandidate, protected []turn.ContextMessageSource) {
	for i := range messages {
		if !s.includes(messages[i].Record.Ref) || isProtectedSource(messages[i], protected) {
			messages[i].Policies = appendPolicy(messages[i].Policies, CompactPolicyMustKeep)
			continue
		}
		messages[i].Policies = slices.DeleteFunc(messages[i].Policies, func(policy CompactPolicy) bool {
			return policy == CompactPolicyPreserveRecent
		})
	}
	propagateMustKeepAcrossToolExchanges(messages)
	for _, group := range toolExchangeGroups(messages) {
		if !completeScopedToolGroup(messages, group) {
			for _, index := range group {
				messages[index].Policies = appendPolicy(messages[index].Policies, CompactPolicyMustKeep)
			}
		}
	}
}

func completeScopedToolGroup(messages []CompactionCandidate, group []int) bool {
	pending := make(map[string]bool)
	seen := make(map[string]bool)
	hasToolPart := false
	for _, index := range group {
		message := historyfrag.StoredModelMessageToSDKMessage(messages[index].Record.ModelMessage)
		for _, part := range message.Content {
			switch part := part.(type) {
			case sdk.ToolCallPart:
				if part.ToolCallID == "" || seen[part.ToolCallID] {
					return false
				}
				seen[part.ToolCallID], pending[part.ToolCallID] = true, true
				hasToolPart = true
			case sdk.ToolResultPart:
				if !pending[part.ToolCallID] {
					return false
				}
				delete(pending, part.ToolCallID)
				hasToolPart = true
			}
		}
	}
	return len(pending) == 0 && (hasToolPart || !messages[group[0]].HasPolicy(CompactPolicyPreserveToolClosure))
}
