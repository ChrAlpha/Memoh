package timeline

import (
	"container/list"
	"sync"
)

// accountedMessages remembers, for the sessions composed most recently, how
// each one's active history checkpoint accounts for the inbound messages a
// composition asked about, by the rows at or before its position. That holds
// for as long as the checkpoint stays active (CheckpointSource.folded), so a
// session's entry lives until the session moves to another checkpoint.
type accountedMessages struct {
	mu       sync.Mutex
	sessions map[string]*list.Element
	lru      *list.List
	max      int
}

// accountedSession is what one session's checkpoint was read to account for.
type accountedSession struct {
	sessionID    string
	checkpointID string

	mu       sync.Mutex
	messages map[string]accountedMessage
}

// accountedMessage is how a checkpoint accounts for an inbound message by its
// context rows at or before the checkpoint's position.
type accountedMessage struct {
	// folds reports that it has such rows and the checkpoint keeps none of
	// them by reference.
	folds bool
	// eventCursor is the cursor of the newest event they were written from.
	eventCursor int64
}

func newAccountedMessages(maxSessions int) *accountedMessages {
	return &accountedMessages{sessions: make(map[string]*list.Element), lru: list.New(), max: maxSessions}
}

// session returns the session's entry for its active checkpoint, a new one
// when the session has none for it, evicting the least recently composed
// session past the bound.
func (a *accountedMessages) session(sessionID, checkpointID string) *accountedSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	if element, ok := a.sessions[sessionID]; ok {
		if entry := element.Value.(*accountedSession); entry.checkpointID == checkpointID {
			a.lru.MoveToFront(element)
			return entry
		}
		a.lru.Remove(element)
		delete(a.sessions, sessionID)
	}
	entry := &accountedSession{sessionID: sessionID, checkpointID: checkpointID, messages: make(map[string]accountedMessage)}
	a.sessions[sessionID] = a.lru.PushFront(entry)
	for a.max > 0 && a.lru.Len() > a.max {
		oldest := a.lru.Back()
		a.lru.Remove(oldest)
		delete(a.sessions, oldest.Value.(*accountedSession).sessionID)
	}
	return entry
}

// fold reports the messages of ids the checkpoint folds by what is known of
// them, leaving out the ones with rows after its position (raw), and names
// the messages nothing is known of yet.
func (e *accountedSession) fold(ids []string, raw map[string]struct{}) (map[string]FoldedMessage, []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	folded := make(map[string]FoldedMessage)
	var unknown []string
	for _, id := range ids {
		if _, ok := raw[id]; ok {
			continue
		}
		message, ok := e.messages[id]
		switch {
		case !ok:
			unknown = append(unknown, id)
		case message.folds:
			folded[id] = FoldedMessage{EventCursor: message.eventCursor}
		}
	}
	return folded, unknown
}

// record adds what was read of messages, and forgets the messages the
// composition no longer asks about once they outnumber the ones it does.
func (e *accountedSession) record(read map[string]accountedMessage, ids []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, message := range read {
		e.messages[id] = message
	}
	if len(e.messages) <= 2*len(ids) {
		return
	}
	asked := make(map[string]accountedMessage, len(ids))
	for _, id := range ids {
		if message, ok := e.messages[id]; ok {
			asked[id] = message
		}
	}
	e.messages = asked
}
