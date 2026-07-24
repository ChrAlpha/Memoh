package timeline

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
)

const charsPerToken = 2

// TurnResponseEntry represents an assistant or tool message from bot_history_messages,
// used as the "TR" stream in context composition.
type TurnResponseEntry struct {
	RequestedAtMs   int64           `json:"requested_at_ms"`
	Role            string          `json:"role"`
	Content         string          `json:"content"`
	RawContent      json.RawMessage `json:"raw_content,omitempty"`
	SourceMessageID string          `json:"source_message_id,omitempty"`
}

// ContextMessage is a unified message for LLM context, produced by MergeContext.
type ContextMessage struct {
	Role                 string          `json:"role"`
	Content              string          `json:"content"`
	RawContent           json.RawMessage `json:"raw_content,omitempty"`
	CompactionArtifactID string          `json:"compaction_artifact_id,omitempty"`
}

// ComposeContextResult holds the output of ComposeContext.
type ComposeContextResult struct {
	Messages        []ContextMessage
	EstimatedTokens int
}

// CompactionSource identifies one durable history source covered by an active
// compaction artifact. ExternalMessageID projects that source onto the rendered
// stream; HistoryMessageID projects it onto persisted turn responses.
type CompactionSource struct {
	HistoryMessageID  string `json:"history_message_id,omitempty"`
	ExternalMessageID string `json:"external_message_id,omitempty"`
	CreatedAtMs       int64  `json:"created_at_ms,omitempty"`
}

// CompactionArtifact is the timeline-facing projection of one active compaction
// artifact. Callers preserve frontier order; composition keeps each artifact
// separate so later restacks can supersede only the ranges they actually cover.
type CompactionArtifact struct {
	ID            string             `json:"id"`
	Summary       string             `json:"summary"`
	AnchorStartMs int64              `json:"anchor_start_ms,omitempty"`
	CompletedAtMs int64              `json:"completed_at_ms,omitempty"`
	Sources       []CompactionSource `json:"sources,omitempty"`
}

// LatestExternalEventMs returns the receivedAtMs of the latest non-self segment
// after afterMs, or 0 if none found.
func LatestExternalEventMs(rc RenderedContext, afterMs int64) int64 {
	var latest int64
	for _, seg := range rc {
		if seg.ReceivedAtMs > afterMs && !seg.IsMyself {
			if seg.ReceivedAtMs > latest {
				latest = seg.ReceivedAtMs
			}
		}
	}
	return latest
}

const earliestMergeTime int64 = -1 << 63

type mergeEntry struct {
	kind string // "summary_before_rc", "rc", "summary", or "tr"
	time int64
	step int
	// For RC entries
	rcContent []RenderedContentPiece
	// For summary entries
	summaryContent    string
	summaryArtifactID string
	// For TR entries
	trRole       string
	trContent    string
	trRawContent json.RawMessage
}

// MergeContext interleaves RC segments and TR entries by timestamp.
// RC entries use receivedAtMs; TR entries use requestedAtMs.
// Tiebreaker: RC before TR on equal timestamp.
// Consecutive RC entries between TR entries are merged into one user message.
func MergeContext(rc RenderedContext, trs []TurnResponseEntry) []ContextMessage {
	entries := make([]mergeEntry, 0, len(rc)+len(trs))
	entries = appendRenderedContextEntries(entries, rc)
	entries = appendTurnResponseEntries(entries, trs)
	return mergeEntries(entries)
}

func appendRenderedContextEntries(entries []mergeEntry, rc RenderedContext) []mergeEntry {
	for _, seg := range rc {
		entries = append(entries, mergeEntry{
			kind:      "rc",
			time:      seg.ReceivedAtMs,
			step:      -1,
			rcContent: seg.Content,
		})
	}
	return entries
}

func appendTurnResponseEntries(entries []mergeEntry, trs []TurnResponseEntry) []mergeEntry {
	for i, tr := range trs {
		entries = append(entries, mergeEntry{
			kind:         "tr",
			time:         tr.RequestedAtMs,
			step:         i,
			trRole:       tr.Role,
			trContent:    tr.Content,
			trRawContent: tr.RawContent,
		})
	}
	return entries
}

func mergeEntries(entries []mergeEntry) []ContextMessage {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].time != entries[j].time {
			return entries[i].time < entries[j].time
		}
		if entries[i].kind != entries[j].kind {
			return mergeKindOrder(entries[i].kind) < mergeKindOrder(entries[j].kind)
		}
		return entries[i].step < entries[j].step
	})
	return materializeMergeEntries(entries)
}

func mergeKindOrder(kind string) int {
	switch kind {
	case "summary_before_rc":
		return 0
	case "rc":
		return 1
	case "summary":
		return 2
	case "tr":
		return 3
	default:
		return 4
	}
}

func materializeMergeEntries(entries []mergeEntry) []ContextMessage {
	var messages []ContextMessage
	var pendingText strings.Builder

	flushRC := func() {
		if pendingText.Len() > 0 {
			messages = append(messages, ContextMessage{Role: "user", Content: pendingText.String()})
			pendingText.Reset()
		}
	}

	for _, entry := range entries {
		switch entry.kind {
		case "rc":
			for _, piece := range entry.rcContent {
				if piece.Type == "text" {
					if pendingText.Len() > 0 {
						pendingText.WriteByte('\n')
					}
					pendingText.WriteString(piece.Text)
				}
			}
		case "summary", "summary_before_rc":
			flushRC()
			messages = append(messages, ContextMessage{
				Role:                 "user",
				Content:              entry.summaryContent,
				CompactionArtifactID: entry.summaryArtifactID,
			})
		case "tr":
			flushRC()
			messages = append(messages, ContextMessage{
				Role:       entry.trRole,
				Content:    entry.trContent,
				RawContent: entry.trRawContent,
			})
		}
	}
	flushRC()

	return messages
}

// ComposeContext merges un-compacted RC and TR streams.
func ComposeContext(rc RenderedContext, trs []TurnResponseEntry) *ComposeContextResult {
	return ComposeContextWithArtifacts(rc, trs, nil)
}

// ComposeContextWithArtifacts replaces covered RC/TR sources with each active
// artifact at the covered rendered slot when available, or its durable anchor.
func ComposeContextWithArtifacts(rc RenderedContext, trs []TurnResponseEntry, artifacts []CompactionArtifact) *ComposeContextResult {
	coveredMessages := coveredExternalMessages(artifacts)
	activeRC := filterCoveredRenderedContext(rc, coveredMessages)
	activeTRs := filterCoveredTurnResponses(trs, artifacts)
	entries := make([]mergeEntry, 0, len(activeRC)+len(activeTRs)+len(artifacts))
	entries = appendRenderedContextEntries(entries, activeRC)
	for i, artifact := range artifacts {
		if !artifact.usable() {
			continue
		}
		kind := "summary"
		summaryAtMs, precedesRenderedContext := artifactSummaryPlacement(artifact, rc, coveredMessages)
		if precedesRenderedContext {
			kind = "summary_before_rc"
		}
		entries = append(entries, mergeEntry{
			kind:              kind,
			time:              summaryAtMs,
			step:              i,
			summaryContent:    "<summary>\n" + strings.TrimSpace(artifact.Summary) + "\n</summary>",
			summaryArtifactID: artifact.ID,
		})
	}
	entries = appendTurnResponseEntries(entries, activeTRs)
	allMessages := mergeEntries(entries)
	if len(allMessages) == 0 {
		return nil
	}

	return &ComposeContextResult{
		Messages:        allMessages,
		EstimatedTokens: estimateMessagesTokens(allMessages),
	}
}

func artifactSummaryPlacement(
	artifact CompactionArtifact,
	rc RenderedContext,
	coveredMessages map[string]externalMessageCoverage,
) (int64, bool) {
	for _, source := range artifact.Sources {
		if source.CreatedAtMs <= 0 {
			continue
		}
		id := strings.TrimSpace(source.ExternalMessageID)
		if id == "" {
			break
		}
		coverage, covered := coveredMessages[id]
		if !covered {
			break
		}
		for _, segment := range rc {
			if strings.TrimSpace(segment.MessageID) == id {
				return segment.ReceivedAtMs, !renderedSegmentCovered(segment, coverage)
			}
		}
		break
	}
	if artifact.AnchorStartMs <= 0 {
		return earliestMergeTime, false
	}
	return artifact.AnchorStartMs, false
}

func filterCoveredRenderedContext(
	rc RenderedContext,
	coveredMessages map[string]externalMessageCoverage,
) RenderedContext {
	if len(coveredMessages) == 0 {
		return rc
	}
	filtered := make(RenderedContext, 0, len(rc))
	for _, segment := range rc {
		coverage, covered := coveredMessages[strings.TrimSpace(segment.MessageID)]
		if covered && renderedSegmentCovered(segment, coverage) {
			continue
		}
		filtered = append(filtered, segment)
	}
	return filtered
}

func filterCoveredTurnResponses(trs []TurnResponseEntry, artifacts []CompactionArtifact) []TurnResponseEntry {
	covered := make(map[string]struct{})
	for _, artifact := range artifacts {
		if !artifact.usable() {
			continue
		}
		for _, source := range artifact.Sources {
			if id := strings.TrimSpace(source.HistoryMessageID); id != "" {
				covered[id] = struct{}{}
			}
		}
	}
	if len(covered) == 0 {
		return trs
	}
	filtered := make([]TurnResponseEntry, 0, len(trs))
	for _, tr := range trs {
		if _, ok := covered[strings.TrimSpace(tr.SourceMessageID)]; ok {
			continue
		}
		filtered = append(filtered, tr)
	}
	return filtered
}

type externalMessageCoverage struct {
	completedAtMs int64
}

func coveredExternalMessages(artifacts []CompactionArtifact) map[string]externalMessageCoverage {
	covered := make(map[string]externalMessageCoverage)
	for _, artifact := range artifacts {
		if !artifact.usable() {
			continue
		}
		for _, source := range artifact.Sources {
			id := strings.TrimSpace(source.ExternalMessageID)
			if id == "" || source.CreatedAtMs <= 0 {
				continue
			}
			coverage := covered[id]
			if artifact.CompletedAtMs > coverage.completedAtMs {
				coverage.completedAtMs = artifact.CompletedAtMs
			}
			covered[id] = coverage
		}
	}
	return covered
}

// renderedSegmentCovered reports whether coverage may replace the segment. A
// segment edited after the artifact completed carries content the summary has
// never seen and must stay visible.
func renderedSegmentCovered(segment RenderedSegment, coverage externalMessageCoverage) bool {
	if segment.EditedAtMs <= 0 {
		return true
	}
	return coverage.completedAtMs > 0 && segment.EditedAtMs <= coverage.completedAtMs
}

func (artifact CompactionArtifact) usable() bool {
	return strings.TrimSpace(artifact.ID) != "" && strings.TrimSpace(artifact.Summary) != ""
}

func estimateMessagesTokens(messages []ContextMessage) int {
	total := 0
	for _, m := range messages {
		total += estimateMessageTokens(m)
	}
	return total
}

func estimateMessageTokens(m ContextMessage) int {
	if len(m.RawContent) > 0 {
		return int(math.Ceil(float64(len(m.RawContent)) / charsPerToken))
	}
	return int(math.Ceil(float64(len(m.Content)) / charsPerToken))
}
