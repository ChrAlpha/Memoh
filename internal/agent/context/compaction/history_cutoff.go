package compaction

import (
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// HistoryCutoff bounds a compaction to the rows created before a moment. A
// retry or edit replaces the turn at the cutoff, so the summaries and rows
// from that turn on must not feed the summary that stands in for the history
// before it. The bounds match what chat context assembly admits, so the
// summary and the context it replaces are built from the same history.
type HistoryCutoff struct {
	BeforeMs int64
}

// admitsArtifact reports whether a summary covers only history before the
// cutoff. An artifact whose anchor range is unknown is not admitted.
func (c *HistoryCutoff) admitsArtifact(artifact Artifact) bool {
	if c == nil {
		return true
	}
	return artifact.AnchorStartMs > 0 &&
		artifact.AnchorStartMs <= artifact.AnchorEndMs &&
		artifact.AnchorEndMs < c.BeforeMs
}

// rowsBeforeCutoff keeps the oldest-first prefix of rows created before the
// cutoff. Rows are ordered by creation time, so the first row at or after the
// cutoff ends the prefix.
func (c *HistoryCutoff) rowsBeforeCutoff(rows []sqlc.ListUncompactedMessagesBySessionRow) []sqlc.ListUncompactedMessagesBySessionRow {
	if c == nil {
		return rows
	}
	for i, row := range rows {
		if row.CreatedAt.Time.UnixMilli() >= c.BeforeMs {
			return rows[:i]
		}
	}
	return rows
}

// artifactsBeforeCutoff keeps the artifacts the cutoff admits.
func (c *HistoryCutoff) artifactsBeforeCutoff(artifacts []Artifact) []Artifact {
	if c == nil {
		return artifacts
	}
	kept := artifacts[:0:0]
	for _, artifact := range artifacts {
		if c.admitsArtifact(artifact) {
			kept = append(kept, artifact)
		}
	}
	return kept
}
