package timeline

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/felinics/memoh/internal/chat/checkpoint"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// HistoryCheckpoint is the timeline-facing projection of a session's active
// history checkpoint, read for the segments a composition renders. The zero
// value folds nothing.
type HistoryCheckpoint struct {
	// Position is where the turn responses composed after it start.
	Position messagepkg.HistoryPosition
	// Items are what the checkpoint holds, in its order.
	Items []CheckpointItem
	// Folded names the inbound messages of the composed segments whose
	// context rows the checkpoint folds: it leaves none of them raw, after
	// its position or kept by reference.
	Folded map[string]FoldedMessage
}

// FoldedMessage is an inbound message whose context rows a history
// checkpoint folds.
type FoldedMessage struct {
	// EventCursor is the cursor of the newest event its rows were written
	// from, 0 when unknown. A summary reads rows, so it knows the message as
	// of that event.
	EventCursor int64
}

// changedSince reports whether the segment carries a change to the message
// after its rows were written: events past the one they were written from
// or, when that is unknown, an edit.
func (m FoldedMessage) changedSince(segment RenderedSegment) bool {
	if m.EventCursor > 0 {
		return segment.LastEventCursor > m.EventCursor
	}
	return segment.EditedAtMs > 0
}

// CheckpointItem is one item of a HistoryCheckpoint: a summary or a row it
// keeps raw.
type CheckpointItem struct {
	SummaryID string
	Summary   string
	// MessageID names the inbound message a kept user row records: the
	// segment of that message renders for it.
	MessageID string
	// Response is what a kept assistant or tool row renders as.
	Response *TurnResponseEntry
}

// composes reports whether a segment the checkpoint does not hold composes
// after it: one whose message has no context rows, or one whose rows the
// checkpoint leaves raw, or one whose rows a summary folded when the message
// changed since. A summary never read such a change, so the segment carries
// it.
func (c HistoryCheckpoint) composes(segment RenderedSegment) bool {
	folded, ok := c.Folded[strings.TrimSpace(segment.MessageID)]
	return !ok || folded.changedSince(segment)
}

// CheckpointRows reads the rows a history checkpoint keeps by reference.
type CheckpointRows interface {
	ListTurnResponseSourcesByIDs(ctx context.Context, sessionID string, ids []string) ([]messagepkg.Message, error)
}

// CheckpointSource reads a session's active history checkpoint as
// composition renders it.
type CheckpointSource struct {
	checkpoints *checkpoint.Store
	queries     dbstore.Queries
	rows        CheckpointRows
	accounted   *accountedMessages
}

// NewCheckpointSource creates a CheckpointSource.
func NewCheckpointSource(log *slog.Logger, queries dbstore.Queries, rows CheckpointRows) *CheckpointSource {
	return &CheckpointSource{
		checkpoints: checkpoint.NewStore(log, queries),
		queries:     queries,
		rows:        rows,
		// As many sessions as the pipeline keeps rendered contexts of.
		accounted: newAccountedMessages(defaultPipelineMaxSessions),
	}
}

// Load reads the session's active checkpoint for composing rc: what it
// holds, the rows it keeps as they render, and which inbound messages of rc
// it folds. A kept user row that records no inbound message renders
// nothing, as no such row reaches composition otherwise.
func (s *CheckpointSource) Load(ctx context.Context, sessionID string, rc RenderedContext) (HistoryCheckpoint, error) {
	active, err := s.checkpoints.Active(ctx, sessionID)
	if err != nil || active.ID == "" {
		return HistoryCheckpoint{}, err
	}
	projection := HistoryCheckpoint{Position: active.Position}
	kept := make(map[string]messagepkg.Message)
	if rows := active.RowIDs(); len(rows) > 0 {
		messages, err := s.rows.ListTurnResponseSourcesByIDs(ctx, sessionID, rows)
		if err != nil {
			return HistoryCheckpoint{}, fmt.Errorf("load history checkpoint rows: %w", err)
		}
		for _, message := range messages {
			kept[message.ID] = message
		}
	}
	for _, item := range active.Items {
		if item.SummaryID != "" {
			projection.Items = append(projection.Items, CheckpointItem{SummaryID: item.SummaryID, Summary: item.Summary})
			continue
		}
		message, ok := kept[item.RowID]
		if !ok {
			return HistoryCheckpoint{}, fmt.Errorf("history checkpoint %s keeps row %s, which is not a context row", active.ID, item.RowID)
		}
		if strings.EqualFold(strings.TrimSpace(message.Role), "user") {
			if message.ExternalMessageID != "" {
				projection.Items = append(projection.Items, CheckpointItem{MessageID: message.ExternalMessageID})
			}
			continue
		}
		if response, ok := DecodeTurnResponseEntry(message); ok {
			projection.Items = append(projection.Items, CheckpointItem{Response: &response})
		}
	}
	ids := inboundMessageIDs(rc)
	if len(ids) == 0 {
		return projection, nil
	}
	if projection.Folded, err = s.folded(ctx, sessionID, active, ids); err != nil {
		return HistoryCheckpoint{}, err
	}
	return projection, nil
}

// folded reports which of the inbound messages ids the checkpoint folds: the
// ones with context rows at or before its position, none of them kept by
// reference, and none after it.
//
// Rows after the position change with every write, so they are read each
// time, walking only the history after the position. What the rows at or
// before it say holds while the checkpoint stays active: rows there are
// hidden, replaced or deleted only by moving the session off it, and none
// land there later, as positions only grow and the one run a session admits
// at a time folds only what is before its own turn. So each message's are
// read once per checkpoint, and a trigger reads only those of the messages
// new to it.
func (s *CheckpointSource) folded(ctx context.Context, sessionID string, active checkpoint.Checkpoint, ids []string) (map[string]FoldedMessage, error) {
	pgSessionID, err := dbpkg.ParseUUID(sessionID)
	if err != nil {
		return nil, err
	}
	after, err := s.queries.ListHistoryMessageSourcesAfter(ctx, sqlc.ListHistoryMessageSourcesAfterParams{
		SessionID:           pgSessionID,
		AfterTurnPosition:   active.Position.Turn,
		AfterTurnMessageSeq: active.Position.Seq,
	})
	if err != nil {
		return nil, fmt.Errorf("list history messages after the checkpoint: %w", err)
	}
	raw := make(map[string]struct{}, len(after))
	for _, id := range after {
		raw[id] = struct{}{}
	}
	known := s.accounted.session(sessionID, active.ID)
	folded, unknown := known.fold(ids, raw)
	if len(unknown) == 0 {
		return folded, nil
	}
	rows, err := s.queries.ListHistoryCheckpointAccountedMessages(ctx, sqlc.ListHistoryCheckpointAccountedMessagesParams{
		KeptIds:               active.RowUUIDs(),
		SessionID:             pgSessionID,
		ExternalMessageIds:    unknown,
		ThroughTurnPosition:   active.Position.Turn,
		ThroughTurnMessageSeq: active.Position.Seq,
	})
	if err != nil {
		return nil, fmt.Errorf("list messages the history checkpoint accounts for: %w", err)
	}
	read := make(map[string]accountedMessage, len(unknown))
	for _, id := range unknown {
		read[id] = accountedMessage{}
	}
	for _, row := range rows {
		read[row.ExternalMessageID] = accountedMessage{folds: !row.Kept, eventCursor: row.EventCursor}
	}
	known.record(read, ids)
	for id, message := range read {
		if message.folds {
			folded[id] = FoldedMessage{EventCursor: message.eventCursor}
		}
	}
	return folded, nil
}

// inboundMessageIDs names the inbound messages of rc.
func inboundMessageIDs(rc RenderedContext) []string {
	ids := make([]string, 0, len(rc))
	for _, segment := range rc {
		if id := strings.TrimSpace(segment.MessageID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}
