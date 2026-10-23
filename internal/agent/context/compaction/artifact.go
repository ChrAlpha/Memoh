package compaction

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
)

const ArtifactVersion = 1

type CoveredSource struct {
	Ref                    contextfrag.ContextRef `json:"ref"`
	ExternalMessageID      string                 `json:"external_message_id,omitempty"`
	SourceReplyToMessageID string                 `json:"source_reply_to_message_id,omitempty"`
	CreatedAtMs            int64                  `json:"created_at_ms,omitempty"`
	CoverageAsOfMs         *int64                 `json:"coverage_as_of_ms,omitempty"`
	EventCursor            int64                  `json:"event_cursor,omitempty"`
}

// Artifact is one persisted compaction summary. FloorMs, when set, is the
// floor it stands on: the rows its lineage claimed up to then are left out of
// the request unsummarized, and Coverage does not list them. FloorNotice
// opens Summary and says so.
type Artifact struct {
	ID                string
	BotID             string
	SessionID         string
	Status            string
	Summary           string
	Version           int
	Coverage          []CoveredSource
	AnchorStartMs     int64
	AnchorEndMs       int64
	FloorMs           int64
	FloorNotice       string
	Level             int
	ParentIDs         []string
	SupersededBy      string
	SupersededAt      time.Time
	StartedAt         time.Time
	CoverageMalformed bool
}

// EndsBefore reports whether the summary covers only rows created before
// the cutoff, so history cut there can still render it. It matches the rule
// memoh_rewind_compaction keeps summaries by.
func (a Artifact) EndsBefore(cutoff time.Time) bool {
	return a.AnchorStartMs > 0 && a.AnchorStartMs <= a.AnchorEndMs && a.AnchorEndMs < cutoff.UnixMilli()
}

func (a Artifact) HistoryRecord(scope contextfrag.Scope) historyfrag.HistoryRecord {
	if scope.BotID == "" {
		scope.BotID = a.BotID
	}
	if scope.SessionID == "" {
		scope.SessionID = a.SessionID
	}
	coveredRefs := make([]contextfrag.ContextRef, 0, len(a.Coverage))
	for _, source := range a.Coverage {
		coveredRefs = append(coveredRefs, source.Ref)
	}
	record := historyfrag.SummaryRecord(a.ID, a.Summary, coveredRefs, scope)
	record.BotID = a.BotID
	record.SessionID = a.SessionID
	record.SessionIDKnown = true
	if a.AnchorEndMs > 0 {
		record.CreatedAt = time.UnixMilli(a.AnchorEndMs).UTC()
	}
	return record
}

// OmitsRecord reports whether a row the summary's lineage claimed lies under
// its floor, which leaves the row out of the request without listing it.
func (a Artifact) OmitsRecord(record historyfrag.HistoryRecord) bool {
	return a.FloorMs > 0 && !record.CreatedAt.IsZero() && record.CreatedAt.UnixMilli() <= a.FloorMs
}

// SummaryBody is the summary without the floor notice that opens it: what a
// summarizer folding it reads.
func (a Artifact) SummaryBody() string {
	return strings.TrimSpace(strings.TrimPrefix(a.Summary, a.FloorNotice))
}

// withFloorNotice opens a summary with the notice of the floor it stands on.
func withFloorNotice(notice, summary string) string {
	if notice == "" {
		return summary
	}
	if summary == "" {
		return notice
	}
	return notice + "\n\n" + summary
}

func (a Artifact) CoversRecord(record historyfrag.HistoryRecord) bool {
	createdAtMs := int64(0)
	if !record.CreatedAt.IsZero() {
		createdAtMs = record.CreatedAt.UnixMilli()
	}
	for _, source := range a.Coverage {
		if compatibleCoverageRef(record.Ref, source.Ref) &&
			strings.TrimSpace(source.ExternalMessageID) == strings.TrimSpace(record.ExternalMessageID) &&
			strings.TrimSpace(source.SourceReplyToMessageID) == strings.TrimSpace(record.SourceReplyToMessageID) &&
			source.CreatedAtMs == createdAtMs {
			return true
		}
	}
	return false
}

type artifactMetadata struct {
	Coverage      []byte
	AnchorStartMs int64
	AnchorEndMs   int64
	FloorMs       int64
	FloorNotice   string
}

func artifactMetadataFor(items []CompactionCandidate, ids []pgtype.UUID) (artifactMetadata, error) {
	byID := make(map[pgtype.UUID]CompactionCandidate, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	covered := make([]CoveredSource, 0, len(ids))
	for _, id := range ids {
		item, ok := byID[id]
		if !ok {
			return artifactMetadata{}, fmt.Errorf("compaction artifact: marked id %s missing from candidates", formatUUID(id))
		}
		createdAtMs := int64(0)
		if !item.Record.CreatedAt.IsZero() {
			createdAtMs = item.Record.CreatedAt.UnixMilli()
		}
		covered = append(covered, CoveredSource{
			Ref:                    item.Record.Ref,
			ExternalMessageID:      item.Record.ExternalMessageID,
			SourceReplyToMessageID: item.Record.SourceReplyToMessageID,
			CreatedAtMs:            createdAtMs,
			EventCursor:            item.EventCursor,
		})
	}
	sort.SliceStable(covered, func(i, j int) bool {
		return covered[i].CreatedAtMs < covered[j].CreatedAtMs
	})
	encoded, err := json.Marshal(covered)
	if err != nil {
		return artifactMetadata{}, fmt.Errorf("encode compaction artifact coverage: %w", err)
	}
	metadata := artifactMetadata{Coverage: encoded}
	if len(covered) > 0 {
		metadata.AnchorStartMs = covered[0].CreatedAtMs
		metadata.AnchorEndMs = covered[len(covered)-1].CreatedAtMs
	}
	return metadata, nil
}

// rollupArtifactMetadata is what a summary folding parents covers: their
// coverage and current's, on the highest floor any of them stands on. A
// floor's anchors span the rows it left out, which no coverage lists.
func rollupArtifactMetadata(parents []Artifact, current artifactMetadata) (artifactMetadata, error) {
	currentCoverage, err := DecodeArtifactCoverage(current.Coverage)
	if err != nil {
		return artifactMetadata{}, err
	}
	covered := make([]CoveredSource, 0, len(currentCoverage))
	for _, parent := range parents {
		if len(parent.Coverage) == 0 && parent.FloorMs == 0 {
			return artifactMetadata{}, fmt.Errorf("%w: artifact_id=%s", errIncompleteAbsorbedCoverage, parent.ID)
		}
		for _, source := range parent.Coverage {
			if source.CoverageAsOfMs == nil {
				asOf := int64(0)
				if !parent.StartedAt.IsZero() {
					asOf = parent.StartedAt.UnixMilli()
				}
				source.CoverageAsOfMs = &asOf
			}
			covered = append(covered, source)
		}
	}
	covered = append(covered, currentCoverage...)
	sort.SliceStable(covered, func(i, j int) bool {
		return covered[i].CreatedAtMs < covered[j].CreatedAtMs
	})
	if err := validatePersistedArtifactCoverage(covered); err != nil {
		return artifactMetadata{}, fmt.Errorf("validate compaction rollup coverage: %w", err)
	}
	encoded, err := json.Marshal(covered)
	if err != nil {
		return artifactMetadata{}, fmt.Errorf("encode compaction rollup coverage: %w", err)
	}
	metadata := artifactMetadata{Coverage: encoded}
	if len(covered) > 0 {
		metadata.AnchorStartMs = covered[0].CreatedAtMs
		metadata.AnchorEndMs = covered[len(covered)-1].CreatedAtMs
	}
	metadata.FloorMs, metadata.FloorNotice = standingFloor(parents)
	for _, parent := range parents {
		if parent.FloorMs > 0 && (metadata.AnchorStartMs == 0 || parent.AnchorStartMs < metadata.AnchorStartMs) {
			metadata.AnchorStartMs = parent.AnchorStartMs
		}
	}
	metadata.AnchorEndMs = max(metadata.AnchorEndMs, metadata.FloorMs)
	return metadata, nil
}

// standingFloor is the highest floor any of parents stands on: the floor a
// summary folding them stands on, and the notice that opens it.
func standingFloor(parents []Artifact) (int64, string) {
	var floorMs int64
	var notice string
	for _, parent := range parents {
		if parent.FloorMs > floorMs {
			floorMs, notice = parent.FloorMs, parent.FloorNotice
		}
	}
	return floorMs, notice
}

func DecodeArtifactCoverage(raw []byte) ([]CoveredSource, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var covered []CoveredSource
	if err := json.Unmarshal(raw, &covered); err != nil {
		return nil, fmt.Errorf("decode compaction artifact coverage: %w", err)
	}
	if err := validatePersistedArtifactCoverage(covered); err != nil {
		return nil, fmt.Errorf("decode compaction artifact coverage: %w", err)
	}
	return covered, nil
}

func validatePersistedArtifactCoverage(covered []CoveredSource) error {
	seen := make(map[string]struct{}, len(covered))
	for i, source := range covered {
		if err := validatePersistedCoverageRef(source.Ref); err != nil {
			return fmt.Errorf("ref %d: %w", i, err)
		}
		key := source.Ref.StableKey()
		if _, ok := seen[key]; ok {
			return fmt.Errorf("ref %d: duplicate stable key %q", i, key)
		}
		seen[key] = struct{}{}
		if i > 0 && source.CreatedAtMs < covered[i-1].CreatedAtMs {
			return fmt.Errorf(
				"ref %d: created_at_ms %d precedes ref %d created_at_ms %d",
				i,
				source.CreatedAtMs,
				i-1,
				covered[i-1].CreatedAtMs,
			)
		}
	}
	return nil
}

func validatePersistedCoverageRef(ref contextfrag.ContextRef) error {
	if err := contextfrag.ValidateContextRef(ref); err != nil {
		return err
	}
	if ref.HashAlgo != contextfrag.HashAlgoSHA256 {
		return fmt.Errorf("hash algo must be %q", contextfrag.HashAlgoSHA256)
	}
	if ref.HashScope != contextfrag.HashScopeSourcePayload {
		return fmt.Errorf("hash scope must be %q", contextfrag.HashScopeSourcePayload)
	}
	if strings.TrimSpace(ref.ContentHash) == "" {
		return errors.New("content hash is required")
	}
	return nil
}
