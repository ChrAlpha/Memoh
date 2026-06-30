package compaction

import (
	"encoding/json"
	"strings"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/historyfrag"
	"github.com/memohai/memoh/internal/userinput"
)

type CompactPolicy string

const (
	CompactPolicyCanDrop             CompactPolicy = "can_drop"
	CompactPolicyPreserveRecent      CompactPolicy = "preserve_recent"
	CompactPolicyPreserveToolClosure CompactPolicy = "preserve_tool_closure"
	CompactPolicyMustKeep            CompactPolicy = "must_keep"
)

type RecordCompactionCandidate struct {
	Record   historyfrag.HistoryRecord
	Policies []CompactPolicy
}

func (c RecordCompactionCandidate) HasPolicy(policy CompactPolicy) bool {
	for _, p := range c.Policies {
		if p == policy {
			return true
		}
	}
	return false
}

func recordCandidatesFromRecords(records []historyfrag.HistoryRecord) []RecordCompactionCandidate {
	candidates := make([]RecordCompactionCandidate, 0, len(records))
	for _, record := range records {
		candidates = append(candidates, RecordCompactionCandidate{
			Record:   record,
			Policies: recordCandidatePolicies(record),
		})
	}
	if len(candidates) > 0 {
		markRecordSelectionPolicies(candidates)
	}
	return candidates
}

func markRecordSelectionPolicies(candidates []RecordCompactionCandidate) {
	if latestUser := latestRecordUserIndex(candidates); latestUser == 0 && len(candidates) > 1 {
		tailStart := recentRecordTailProtectedStart(candidates, 1)
		for i := range candidates {
			if i == 0 || i >= tailStart {
				candidates[i].Policies = appendCompactPolicy(candidates[i].Policies, CompactPolicyPreserveRecent)
				continue
			}
			if candidates[i].HasPolicy(CompactPolicyMustKeep) {
				candidates[i].Policies = appendCompactPolicy(candidates[i].Policies, CompactPolicyPreserveRecent)
				continue
			}
			candidates[i].Policies = appendCompactPolicy(candidates[i].Policies, CompactPolicyCanDrop)
		}
		return
	}

	start := recentRecordProtectedStart(candidates)
	for i := range candidates {
		if candidates[i].HasPolicy(CompactPolicyMustKeep) {
			candidates[i].Policies = appendCompactPolicy(candidates[i].Policies, CompactPolicyPreserveRecent)
			continue
		}
		if i < start {
			candidates[i].Policies = appendCompactPolicy(candidates[i].Policies, CompactPolicyCanDrop)
			continue
		}
		candidates[i].Policies = appendCompactPolicy(candidates[i].Policies, CompactPolicyPreserveRecent)
	}
}

func recentRecordProtectedStart(candidates []RecordCompactionCandidate) int {
	if len(candidates) == 0 {
		return 0
	}
	if latestUser := latestRecordUserIndex(candidates); latestUser >= 0 {
		return latestUser
	}
	return recentRecordTailProtectedStart(candidates, 0)
}

func latestRecordUserIndex(candidates []RecordCompactionCandidate) int {
	for i := len(candidates) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(candidates[i].Record.ModelMessage.Role), "user") {
			return i
		}
	}
	return -1
}

func recentRecordTailProtectedStart(candidates []RecordCompactionCandidate, minStart int) int {
	start := len(candidates) - 1
	for start > minStart && isRecordToolClosureResult(candidates[start]) {
		start--
	}
	return start
}

func recordCandidatePolicies(record historyfrag.HistoryRecord) []CompactPolicy {
	var policies []CompactPolicy
	if isRecordToolExchange(record) {
		policies = appendCompactPolicy(policies, CompactPolicyPreserveToolClosure)
	}
	if isRecordAskUser(record) {
		policies = appendCompactPolicy(policies, CompactPolicyMustKeep)
		policies = appendCompactPolicy(policies, CompactPolicyPreserveRecent)
		policies = appendCompactPolicy(policies, CompactPolicyPreserveToolClosure)
	}
	return policies
}

func appendCompactPolicy(policies []CompactPolicy, policy CompactPolicy) []CompactPolicy {
	for _, existing := range policies {
		if existing == policy {
			return policies
		}
	}
	return append(policies, policy)
}

func isRecordToolExchange(record historyfrag.HistoryRecord) bool {
	mm := record.ModelMessage
	if strings.EqualFold(strings.TrimSpace(mm.Role), "tool") {
		return true
	}
	if len(mm.ToolCalls) > 0 {
		return true
	}
	for _, part := range parseRecordEntryParts(mm.Content) {
		if strings.Contains(part.Type, "tool-call") ||
			strings.Contains(part.Type, "tool_call") ||
			strings.Contains(part.Type, "tool-result") ||
			strings.Contains(part.Type, "tool_result") {
			return true
		}
	}
	return false
}

func isRecordAskUser(record historyfrag.HistoryRecord) bool {
	mm := record.ModelMessage
	if isAskUserToolName(mm.Name) {
		return true
	}
	for _, call := range mm.ToolCalls {
		if isAskUserToolName(call.Function.Name) {
			return true
		}
	}
	for _, part := range parseRecordEntryParts(mm.Content) {
		if isAskUserToolName(part.ToolName) {
			return true
		}
	}
	return false
}

func isAskUserToolName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), userinput.ToolNameAskUser)
}

func estimateRecordCandidateTokens(candidate RecordCompactionCandidate) int {
	if candidate.Record.UsageOutputTokens != nil && *candidate.Record.UsageOutputTokens > 0 {
		return *candidate.Record.UsageOutputTokens
	}
	raw, _ := json.Marshal(candidate.Record.ModelMessage.Content)
	return len(raw) / 4
}

func estimateRecordCompactPromptTokens(candidate RecordCompactionCandidate) int {
	tokens := estimateRecordCandidateTokens(candidate)
	if header := renderRecordEntryHeader(candidate.Record); header != "" {
		tokens += estimateBytesAsTokens(header)
	}
	return tokens
}

func estimateBytesAsTokens(value string) int {
	if value == "" {
		return 0
	}
	return (len(value) + 3) / 4
}

func splitRecordCandidatesByRatio(candidates []RecordCompactionCandidate, totalInputTokens, ratio int) []RecordCompactionCandidate {
	if ratio <= 0 || totalInputTokens <= 0 || len(candidates) == 0 {
		return nil
	}
	if ratio >= 100 {
		return guardedRecordCompactionCandidates(candidates, len(candidates))
	}

	keepTokens := totalInputTokens * (100 - ratio) / 100
	if keepTokens <= 0 {
		return guardedRecordCompactionCandidates(candidates, len(candidates))
	}

	accumulated := 0
	cutoff := len(candidates)
	for i := len(candidates) - 1; i >= 0; i-- {
		accumulated += estimateRecordCandidateTokens(candidates[i])
		if accumulated >= keepTokens {
			cutoff = i + 1
			break
		}
	}
	if cutoff <= 0 {
		return nil
	}
	return guardedRecordCompactionCandidates(candidates, cutoff)
}

func splitRecordCandidatesByTarget(candidates []RecordCompactionCandidate, targetTokens int) []RecordCompactionCandidate {
	if targetTokens <= 0 || len(candidates) == 0 {
		return nil
	}
	accumulated := 0
	cutoff := 0
	for i := len(candidates) - 1; i >= 0; i-- {
		accumulated += estimateRecordCandidateTokens(candidates[i])
		if accumulated > targetTokens {
			cutoff = i + 1
			break
		}
	}
	if cutoff <= 0 {
		return nil
	}
	return guardedRecordCompactionCandidates(candidates, cutoff)
}

func guardedRecordCompactionCandidates(candidates []RecordCompactionCandidate, cutoff int) []RecordCompactionCandidate {
	if cutoff <= 0 || len(candidates) == 0 {
		return nil
	}
	protectedStart := firstRecordPolicyStart(candidates, CompactPolicyPreserveRecent)
	if protectedStart <= 0 {
		return guardedCurrentTurnRecordCompactionCandidates(candidates, cutoff)
	}
	if cutoff > protectedStart {
		cutoff = protectedStart
	}
	cutoff = adjustRecordToolBoundary(candidates, cutoff)
	if cutoff > protectedStart || cutoff <= 0 {
		return nil
	}
	return candidates[:cutoff]
}

func guardedCurrentTurnRecordCompactionCandidates(candidates []RecordCompactionCandidate, cutoff int) []RecordCompactionCandidate {
	if len(candidates) <= 1 {
		return nil
	}
	protectedTailStart := firstRecordPolicyStartAfter(candidates, CompactPolicyPreserveRecent, 1)
	if protectedTailStart <= 1 {
		return nil
	}
	if cutoff > protectedTailStart {
		cutoff = protectedTailStart
	}
	if cutoff <= 1 {
		return nil
	}
	cutoff = adjustRecordToolBoundary(candidates, cutoff)
	if cutoff > protectedTailStart {
		return nil
	}
	return candidates[1:cutoff]
}

func firstRecordPolicyStart(candidates []RecordCompactionCandidate, policy CompactPolicy) int {
	return firstRecordPolicyStartAfter(candidates, policy, 0)
}

func firstRecordPolicyStartAfter(candidates []RecordCompactionCandidate, policy CompactPolicy, start int) int {
	if start < 0 {
		start = 0
	}
	for i, candidate := range candidates {
		if i < start {
			continue
		}
		if candidate.HasPolicy(policy) {
			return i
		}
	}
	return len(candidates)
}

func adjustRecordToolBoundary(candidates []RecordCompactionCandidate, cutoff int) int {
	for cutoff > 0 && cutoff < len(candidates) && isRecordToolClosureResult(candidates[cutoff]) {
		cutoff++
	}
	return cutoff
}

func isRecordToolClosureResult(candidate RecordCompactionCandidate) bool {
	return candidate.HasPolicy(CompactPolicyPreserveToolClosure) && isRecordToolResult(candidate)
}

func isRecordToolResult(candidate RecordCompactionCandidate) bool {
	return strings.EqualFold(strings.TrimSpace(candidate.Record.ModelMessage.Role), "tool")
}

func buildRecordEntriesAndRefs(candidates []RecordCompactionCandidate) ([]messageEntry, []contextfrag.ContextRef) {
	entries := make([]messageEntry, 0, len(candidates))
	refs := make([]contextfrag.ContextRef, 0, len(candidates))
	for _, candidate := range candidates {
		refs = append(refs, candidate.Record.Ref)
		content := renderRecordCandidateEntry(candidate.Record)
		if strings.TrimSpace(content) == "" {
			continue
		}
		entries = append(entries, messageEntry{
			Role:    candidate.Record.ModelMessage.Role,
			Content: content,
		})
	}
	return entries, refs
}

func trimRecordCompactCandidates(candidates []RecordCompactionCandidate, maxTokens int) []RecordCompactionCandidate {
	if len(candidates) == 0 || maxTokens <= 0 {
		return candidates
	}
	total := 0
	for _, candidate := range candidates {
		total += estimateRecordCompactPromptTokens(candidate)
	}
	if total <= maxTokens {
		return candidates
	}
	accumulated := 0
	cutoff := len(candidates)
	for i := len(candidates) - 1; i >= 0; i-- {
		accumulated += estimateRecordCompactPromptTokens(candidates[i])
		if accumulated > maxTokens {
			cutoff = i + 1
			break
		}
	}
	cutoff = adjustRecordToolBoundary(candidates, cutoff)
	if cutoff >= len(candidates) {
		return candidates
	}
	return candidates[cutoff:]
}
