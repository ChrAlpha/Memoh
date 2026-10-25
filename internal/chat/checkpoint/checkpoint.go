// Package checkpoint keeps the history checkpoints of a session: what stands
// in a request for the history at or before a position, so that only the
// rows after it load raw.
//
// A checkpoint holds, in order, the rows it keeps by reference and the
// summaries that fold the rest, with a record of the history it omits
// without a summary; it holds a summary, an omission, or both. A request
// renders a notice of what it omits, the items in their order, and then the
// session's context rows after the checkpoint's position: its visible rows
// of complete turns, other than legacy passive_sync rows. Compaction folds
// onto the active checkpoint and makes the result active; a fork takes a copy
// of its source's. A request steps over the history its bot recorded before
// keeping history under checkpoints when it cannot render it (StepOver).
// Every checkpoint names its parent, so a change to the history the active
// one depends on moves the session back to the newest one that does not.
package checkpoint

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// Item is one entry of a checkpoint: a row it keeps by reference or a
// summary, named by its compaction log.
type Item struct {
	RowID     string `json:"row,omitempty"`
	SummaryID string `json:"summary,omitempty"`
	// Summary is the summary's text, filled in when the checkpoint is read
	// for a request.
	Summary string `json:"-"`
}

// Omitted is what a checkpoint leaves out without a summary: how many
// context rows, and when the first and the last of them was written. A
// history notice reports it.
type Omitted struct {
	Count int64
	First time.Time
	Last  time.Time
}

// Omits reports whether anything is left out.
func (o Omitted) Omits() bool {
	return o.Count > 0 && !o.First.IsZero() && !o.Last.IsZero()
}

// add is o with rows more left out, written from first to last.
func (o Omitted) add(rows int64, first, last time.Time) Omitted {
	if rows <= 0 {
		return o
	}
	if !o.Omits() {
		return Omitted{Count: rows, First: first, Last: last}
	}
	o.Count += rows
	if first.Before(o.First) {
		o.First = first
	}
	if last.After(o.Last) {
		o.Last = last
	}
	return o
}

// Checkpoint is one history checkpoint of a session. The zero Checkpoint
// stands for nothing: every context row of the session renders raw.
type Checkpoint struct {
	ID        string
	BotID     string
	SessionID string
	ParentID  string
	// Position is the last place in the history the checkpoint accounts for.
	Position  messagepkg.HistoryPosition
	Omitted   Omitted
	Items     []Item
	CreatedAt time.Time
}

// RowIDs names the rows the checkpoint keeps by reference, in item order.
func (c Checkpoint) RowIDs() []string {
	var rows []string
	for _, item := range c.Items {
		if item.RowID != "" {
			rows = append(rows, item.RowID)
		}
	}
	return rows
}

// RowUUIDs names the rows the checkpoint keeps by reference as the queries
// that take them do.
func (c Checkpoint) RowUUIDs() []pgtype.UUID {
	rows := c.RowIDs()
	ids := make([]pgtype.UUID, 0, len(rows))
	for _, id := range rows {
		ids = append(ids, dbpkg.ParseUUIDOrEmpty(id))
	}
	return ids
}

// SummaryIDs names the summaries the checkpoint holds, in item order.
func (c Checkpoint) SummaryIDs() []string {
	var summaries []string
	for _, item := range c.Items {
		if item.SummaryID != "" {
			summaries = append(summaries, item.SummaryID)
		}
	}
	return summaries
}

func fromRow(row sqlc.BotHistoryCheckpoint) (Checkpoint, error) {
	var items []Item
	if err := json.Unmarshal(row.Items, &items); err != nil {
		return Checkpoint{}, fmt.Errorf("decode items of history checkpoint %s: %w", row.ID, err)
	}
	for i, item := range items {
		item.RowID, item.SummaryID = strings.TrimSpace(item.RowID), strings.TrimSpace(item.SummaryID)
		if (item.RowID == "") == (item.SummaryID == "") {
			return Checkpoint{}, fmt.Errorf("history checkpoint %s: item %d names neither one row nor one summary", row.ID, i)
		}
		items[i] = item
	}
	checkpoint := Checkpoint{
		ID:        row.ID.String(),
		BotID:     row.BotID.String(),
		SessionID: row.SessionID.String(),
		ParentID:  row.ParentID.String(),
		Position:  messagepkg.HistoryPosition{Turn: row.TurnPosition, Seq: row.TurnMessageSeq},
		Items:     items,
		CreatedAt: row.CreatedAt.Time,
	}
	if row.OmittedCount > 0 {
		checkpoint.Omitted = Omitted{Count: row.OmittedCount, First: row.OmittedFirstAt.Time, Last: row.OmittedLastAt.Time}
	}
	return checkpoint, nil
}

func encodeItems(items []Item) ([]byte, error) {
	if items == nil {
		items = []Item{}
	}
	return json.Marshal(items)
}
