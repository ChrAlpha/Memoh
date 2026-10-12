package compaction

import (
	"context"
	"errors"
	"log/slog"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

const (
	maxCompactionScanBytes   = 32 << 20
	maxCompactionScanWindows = 64
)

type compactionScan struct {
	state        sqlc.GetCompactionScanRow
	after        pgtype.UUID
	done         bool
	partial      bool
	rows         []sqlc.ListUncompactedMessagesBySessionRow
	items, span  []CompactionCandidate
	behindTokens int
	windows      int
	bytes        int64
}

func closedToolGroup(items []CompactionCandidate, group []int) bool {
	if isToolResultItem(items[group[0]]) {
		return false
	}
	calls, results := map[string]bool{}, map[string]bool{}
	for _, i := range group {
		for _, part := range historyfrag.StoredModelMessageToSDKMessage(items[i].Record.ModelMessage).Content {
			switch p := part.(type) {
			case sdk.ToolCallPart:
				if p.ToolCallID == "" {
					return false
				}
				calls[p.ToolCallID] = true
			case sdk.ToolResultPart:
				results[p.ToolCallID] = true
			}
		}
	}
	if len(calls) != len(results) {
		return false
	}
	for id := range calls {
		if !results[id] {
			return false
		}
	}
	return true
}

func selectCompactionSpan(items []CompactionCandidate, budget int) ([]CompactionCandidate, int) {
	groups := toolExchangeGroups(items)
	start, end := -1, 0
	resume := len(items)
	for g, group := range groups {
		first, last := group[0], group[len(group)-1]+1
		closed := closedToolGroup(items, group)
		eligible := closed && markableGroupCost(items, group) > 0
		for _, i := range group {
			eligible = eligible && !items[i].HasPolicy(CompactPolicyPreserveRecent)
		}
		if start >= 0 && (!eligible || items[first].GapBefore) {
			break
		}
		if eligible {
			if start < 0 {
				start = first
			}
			end = last
		} else if g == len(groups)-1 && !closed && !isToolResultItem(items[first]) {
			resume = first
		}
	}
	if start < 0 {
		return nil, resume
	}
	return trimCompactMessages(items[start:end], budget), resume
}

func unreadReplayTokens(bytes, images int64) int {
	return contextfrag.ProviderBudgetTokensFromBytes(int(max(0, bytes))) + int(max(0, images))*contextfrag.EstimateImageTokens
}

func (s *Service) readCompactionSpan(ctx context.Context, sessionID pgtype.UUID, cfg TriggerConfig, measure sqlc.MeasureUncompactedMessagesBySessionRow, budget int) (*compactionScan, error) {
	state, err := s.queries.GetCompactionScan(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	scan := &compactionScan{state: state}
	if state.CompactionScanEpoch == state.CompactionEpoch {
		scan.after = state.CompactionScanAfter
	}
	protectedIDs, externalIDs := protectedSourceIDs(cfg.ProtectedSources)
	readBytes := compactionReadMaxBytes(cfg)
	for scan.windows < maxCompactionScanWindows && scan.bytes < maxCompactionScanBytes {
		remaining := maxCompactionScanBytes - scan.bytes
		window, err := s.queries.ListUncompactedMessagesBySessionWithinBytes(ctx, sqlc.ListUncompactedMessagesBySessionWithinBytesParams{
			SessionID: sessionID, MaxBytes: min(readBytes, remaining), AfterMessageID: scan.after, ScanBefore: state.Cutoff,
			ProtectedMessageIds: protectedIDs, ProtectedExternalIds: externalIDs,
		})
		if err != nil {
			return nil, err
		}
		if len(window) == 0 {
			scan.done, scan.partial = true, measure.CandidateCount > 0
			if scan.windows == 0 {
				scan.behindTokens = unreadReplayTokens(measure.CandidateBytes-measure.CandidateImageBytes-measure.ProtectedBytes, measure.CandidateImages-measure.ProtectedImages)
			}
			break
		}
		if window[0].CompactionEpoch != state.CompactionEpoch {
			return nil, errors.New("compaction: source epoch changed during scan")
		}
		if scan.windows == 0 {
			scan.behindTokens = unreadReplayTokens(
				measure.CandidateBytes-measure.CandidateImageBytes-measure.ProtectedBytes-(window[0].CandidateBytes-window[0].CandidateImageBytes-window[0].ProtectedBytes),
				measure.CandidateImages-measure.ProtectedImages-(window[0].CandidateImages-window[0].ProtectedImages))
		}
		scan.windows++
		truncated := window[0].CandidateCount > int64(len(window))
		scan.partial = scan.partial || truncated || scan.windows > 1 || window[0].CandidateCount < measure.CandidateCount
		if window[0].Oversized {
			if remaining < readBytes {
				break
			}
			if readBytes < maxCompactionReadBytes {
				readBytes = min(2*readBytes, maxCompactionReadBytes)
				continue
			}
			if !isProtectedRow(uncompactedRowsFromBounded(window)[0], cfg.ProtectedSources) {
				scan.behindTokens += unreadReplayTokens(window[0].CumulativeBytes-window[0].ImageBytes, window[0].ImageParts)
			}
			scan.after = window[0].ID
			if !truncated {
				scan.done = true
				break
			}
			readBytes = compactionReadMaxBytes(cfg)
			continue
		}
		scan.bytes += window[len(window)-1].CumulativeBytes
		scan.rows = uncompactedRowsFromBounded(window)
		scan.items, _ = itemsFromRows(scan.rows)
		for i := range scan.items {
			scan.items[i].GapBefore = window[i].GapBefore
			if truncated {
				scan.items[i].Policies = withoutPolicy(scan.items[i].Policies, CompactPolicyPreserveRecent)
				if window[i].LatestUser {
					scan.items[i].Policies = appendPolicy(scan.items[i].Policies, CompactPolicyPreserveRecent)
				}
			}
		}
		protectCurrentSources(scan.items, cfg.ProtectedSources)
		for i := range window {
			if window[i].ProtectedBefore {
				protectFrom(scan.items, i)
				break
			}
		}
		candidates := scan.items
		if !truncated {
			candidates = splitRecent(candidates, cfg)
		}
		var resume int
		scan.span, resume = selectCompactionSpan(candidates, budget)
		if len(scan.span) == 0 && !truncated {
			if task := latestUserIndex(scan.items); task > 0 {
				current := append([]CompactionCandidate(nil), scan.items[task:]...)
				for i := range current {
					if !current[i].HasPolicy(CompactPolicyMustKeep) {
						current[i].Policies = withoutPolicy(current[i].Policies, CompactPolicyPreserveRecent)
					}
				}
				markSelectionPolicies(current)
				scan.span, _ = selectCompactionSpan(splitRecent(current, cfg), budget)
			}
		}
		if len(scan.span) > 0 {
			scan.after = scan.span[len(scan.span)-1].ID
			return scan, nil
		}
		if !truncated {
			scan.done = true
			break
		}
		if resume == 0 && remaining < readBytes {
			break
		}
		if resume == 0 && readBytes < maxCompactionReadBytes {
			readBytes = min(2*readBytes, maxCompactionReadBytes)
			continue
		}
		if resume == 0 {
			resume = len(scan.items)
		}
		scan.behindTokens += retainedReplayTokens(scan.items[:resume], nil, nil, true, cfg.ProtectedSources)
		scan.after = scan.rows[resume-1].ID
		scan.rows, scan.items = nil, nil
		readBytes = compactionReadMaxBytes(cfg)
	}
	return scan, nil
}

func splitRecent(items []CompactionCandidate, cfg TriggerConfig) []CompactionCandidate {
	if cfg.TargetTokens > 0 {
		return splitByTarget(items, cfg.TargetTokens)
	}
	return splitByRatio(items, cfg.TotalInputTokens, cfg.Ratio)
}

func (s *Service) advanceScan(ctx context.Context, sessionID pgtype.UUID, scan *compactionScan) error {
	after := scan.after
	if scan.done {
		after = pgtype.UUID{}
	}
	n, err := s.queries.AdvanceCompactionScan(ctx, sqlc.AdvanceCompactionScanParams{
		SessionID: sessionID, CompactionEpoch: scan.state.CompactionEpoch,
		PreviousScanEpoch: scan.state.CompactionScanEpoch, PreviousScanAt: scan.state.CompactionScanAt, PreviousAfter: scan.state.CompactionScanAfter,
		AfterMessageID: after, ScanAt: scan.state.Cutoff, RoundComplete: scan.done,
	})
	if err == nil && n != 1 {
		err = errors.New("compaction: scan changed before progress was saved")
	}
	s.logger.InfoContext(ctx, "compaction: scan progress", slog.Int("windows", scan.windows), slog.Int64("scanned_bytes", scan.bytes), slog.Bool("round_complete", scan.done), slog.String("after", formatUUID(after)))
	return err
}
