package runreq

import (
	"errors"
	"strings"
	"time"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/agent/turn/turnwire"
	"github.com/felinics/memoh/internal/auth"
)

// EncodeQueueCommand stores a command the way every queue item holds it: its
// text, the turn it replays and the identities its credentials are issued
// for. The credentials themselves are never stored.
func EncodeQueueCommand(cmd turn.StartTurnCommand, claims *auth.RouteCredentials) ([]byte, error) {
	return turnwire.NewQueuedInput(cmd, claims, time.Time{}).Marshal()
}

// ErrQueueItemNotEditable reports an edit by anyone other than the sender the
// item recorded. A queued command runs as that sender, with their attribution,
// credentials, and memory, so rewriting its text is theirs alone; others with
// queue access can still cancel or reorder it.
var ErrQueueItemNotEditable = errors.New("queue: only the sender can edit a queued item")

// RewriteQueuePayloadText replaces the user text of a stored item while
// keeping the command's sender, routing, and attachment metadata intact. Only
// the recorded sender may edit; an item without a recorded sender user (a
// platform sender with no linked account, or a payload without a command) is
// not editable.
func RewriteQueuePayloadText(payload []byte, editorUserID, text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, sessionruntime.ErrQueueInvalidReference
	}
	body := turnwire.ParseQueuedInput(payload)
	editor := strings.TrimSpace(editorUserID)
	if body.Command == nil || editor == "" || strings.TrimSpace(body.Command.UserID) != editor {
		return nil, ErrQueueItemNotEditable
	}
	body.Text = text
	body.Command.Query = text
	body.Command.ModelQuery = ""
	body.Command.UserVisibleText = text
	return body.Marshal()
}

// QueuePayloadText renders only user-visible text. Invalid/empty payloads never
// fall back to the raw envelope.
func QueuePayloadText(payload []byte) string {
	return turnwire.ParseQueuedInput(payload).VisibleText()
}
