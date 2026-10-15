package turnwire

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/auth"
)

// QueuedInput is the stored form of one input that waits for its session. The
// queue keeps claims and never a credential: Command has its tokens blanked and
// Claims holds the identities they were issued for, so whoever starts the turn
// signs new ones. A token stored with the input would expire while it waits.
type QueuedInput struct {
	// Text is what the queue shows for the input.
	Text string `json:"text"`
	// Command is the turn the input starts. Items written by earlier releases
	// may lack it; they stay listable but cannot be started.
	Command *StartTurn `json:"command,omitempty"`
	// Claims names who the turn answers and acts for. Without it the turn
	// starts with no credentials, as a text typed into the queue panel always
	// did.
	Claims *auth.RouteCredentials `json:"claims,omitempty"`
	// ReceivedAt is when the platform delivered the input. A reply is judged
	// against it, not against the moment the input leaves the queue.
	ReceivedAt time.Time `json:"received_at,omitzero"`
	// ItemID is the queue item the input was read from. It is not stored: the
	// reader sets it, so the turn's earlier messages carry the identity of the
	// items they waited as.
	ItemID string `json:"-"`
}

// NewQueuedInput builds the stored form of cmd. Its credentials are dropped.
func NewQueuedInput(cmd turn.StartTurnCommand, claims *auth.RouteCredentials, receivedAt time.Time) QueuedInput {
	text := strings.TrimSpace(cmd.UserVisibleText)
	if text == "" {
		text = strings.TrimSpace(cmd.Query)
	}
	wire := FromStartTurn(cmd)
	wire.Token = ""
	wire.ChatToken = ""
	return QueuedInput{Text: text, Command: &wire, Claims: claims, ReceivedAt: receivedAt}
}

// Marshal encodes the input for storage.
func (q QueuedInput) Marshal() ([]byte, error) {
	return json.Marshal(q)
}

// ParseQueuedInput reads a stored input. A payload that cannot be read yields
// the zero value, which has no text and no command.
func ParseQueuedInput(payload []byte) QueuedInput {
	var q QueuedInput
	if err := json.Unmarshal(payload, &q); err != nil {
		return QueuedInput{}
	}
	q.Text = strings.TrimSpace(q.Text)
	return q
}

// VisibleText is the text to show for the input, never the raw payload.
func (q QueuedInput) VisibleText() string {
	if q.Text != "" {
		return q.Text
	}
	if q.Command != nil {
		if text := strings.TrimSpace(q.Command.UserVisibleText); text != "" {
			return text
		}
		return strings.TrimSpace(q.Command.Query)
	}
	return ""
}

// ErrQueuedInputWithoutCommand reports a stored input that carries no turn.
var ErrQueuedInputWithoutCommand = errors.New("turnwire: queued input carries no command")

// StartCommand restores the turn the input starts.
func (q QueuedInput) StartCommand() (turn.StartTurnCommand, error) {
	if q.Command == nil {
		return turn.StartTurnCommand{}, ErrQueuedInputWithoutCommand
	}
	return q.Command.Command()
}

// QueuedTurn is what one run answers when it takes a batch of inputs.
type QueuedTurn struct {
	// Command is the turn without credentials, retry identity or defer marker;
	// the caller sets those. Its own message is the last input's, and the
	// earlier inputs are its Preceding messages.
	Command turn.StartTurnCommand
	Claims  *auth.RouteCredentials
	// ReceivedAt is the arrival time of the last input, the one the reply
	// answers.
	ReceivedAt time.Time
}

// MergeQueued builds the single turn that answers a batch. The turn is the last
// input's, so its identity, reply target and quoted message are the ones the
// reply goes to. Every earlier input stays a message of its own, with its
// sender, mentions, reply and attachments: the turn carries them as Preceding
// messages, in arrival order, which the run writes as user rows ahead of its own
// request. A batch of one is that input unchanged.
func MergeQueued(inputs []QueuedInput) (QueuedTurn, error) {
	if len(inputs) == 0 {
		return QueuedTurn{}, errors.New("turnwire: empty queued batch")
	}
	commands := make([]turn.StartTurnCommand, len(inputs))
	for i, input := range inputs {
		cmd, err := input.StartCommand()
		if err != nil {
			return QueuedTurn{}, err
		}
		commands[i] = cmd
	}
	last := inputs[len(inputs)-1]
	merged := commands[len(commands)-1]
	for i := range len(commands) - 1 {
		merged.Preceding = append(merged.Preceding, precedingMessage(commands[i], inputs[i].ItemID, inputs[i].ReceivedAt))
	}
	return QueuedTurn{Command: merged, Claims: last.Claims, ReceivedAt: last.ReceivedAt}, nil
}

func precedingMessage(cmd turn.StartTurnCommand, itemID string, receivedAt time.Time) turn.PrecedingMessage {
	message := turn.PrecedingMessage{
		ID:                        itemID,
		ExternalMessageID:         cmd.ExternalMessageID,
		UserID:                    cmd.UserID,
		SourceChannelIdentityID:   cmd.SourceChannelIdentityID,
		DisplayName:               cmd.DisplayName,
		Query:                     cmd.Query,
		UserVisibleText:           cmd.UserVisibleText,
		Attachments:               cmd.Attachments,
		SourceReplyToMessageID:    cmd.SourceReplyToMessageID,
		ReplySender:               cmd.ReplySender,
		ReplyPreview:              cmd.ReplyPreview,
		ReplyAttachments:          cmd.ReplyAttachments,
		MentionsBot:               cmd.MentionsBot,
		RepliesToBot:              cmd.RepliesToBot,
		ForwardMessageID:          cmd.ForwardMessageID,
		ForwardFromUserID:         cmd.ForwardFromUserID,
		ForwardFromConversationID: cmd.ForwardFromConversationID,
		ForwardSender:             cmd.ForwardSender,
		ForwardDate:               cmd.ForwardDate,
	}
	if !receivedAt.IsZero() {
		message.ReceivedAtMs = receivedAt.UnixMilli()
	}
	return message
}
