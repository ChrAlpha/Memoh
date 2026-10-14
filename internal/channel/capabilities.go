package channel

import "time"

// ChannelCapabilities describes the feature matrix of a channel type.
// It is used by the outbound layer to validate message content before delivery.
type ChannelCapabilities struct {
	Text            bool `json:"text"`
	Markdown        bool `json:"markdown"`
	RichText        bool `json:"rich_text"`
	Attachments     bool `json:"attachments"`
	Media           bool `json:"media"`
	Reactions       bool `json:"reactions"`
	Buttons         bool `json:"buttons"`
	URLButtons      bool `json:"url_buttons"`
	Reply           bool `json:"reply"`
	Threads         bool `json:"threads"`
	Streaming       bool `json:"streaming"`
	Polls           bool `json:"polls"`
	Edit            bool `json:"edit"`
	Unsend          bool `json:"unsend"`
	NativeCommands  bool `json:"native_commands"`
	NativeUserInput bool `json:"native_user_input"`
	// NativeApproval means the adapter renders tool-approval requests itself;
	// without it the outbound layer sends the prompt as a plain message.
	NativeApproval bool     `json:"native_approval"`
	BlockStreaming bool     `json:"block_streaming"`
	ChatTypes      []string `json:"chat_types,omitempty"`
	// Preview selects how a streamed reply is shown. The zero value is
	// PreviewLegacy, the behavior every adapter has today.
	Preview PreviewMode `json:"preview,omitempty"`
	// ReplyWindow is how long after an inbound message the platform accepts a
	// passive reply to it. Zero means the platform imposes no such window.
	ReplyWindow time.Duration `json:"reply_window,omitempty"`
	// ProactiveSend reports whether the adapter can still send to the
	// conversation after ReplyWindow has passed.
	ProactiveSend bool `json:"proactive_send,omitempty"`
	// ReactionRemoveKey says how a reaction is removed: by the message it sat
	// on, or by the reaction's own id. The zero value is ReactionRemoveByMessageID.
	ReactionRemoveKey ReactionRemoveKey `json:"reaction_remove_key,omitempty"`
}

// PreviewMode is how an adapter shows a reply while it is still being produced.
type PreviewMode int

const (
	// PreviewLegacy is the Push/Close stream every adapter implements today.
	PreviewLegacy PreviewMode = iota
	// PreviewNone sends the reply only when it is complete.
	PreviewNone
	// PreviewEditInPlace shows the reply by editing one message.
	PreviewEditInPlace
	// PreviewNativeStream shows the reply through the platform's own streaming
	// message API.
	PreviewNativeStream
	// PreviewDraft shows the reply as a platform draft.
	PreviewDraft
)

// ReactionRemoveKey is the identifier a platform needs to remove a reaction.
type ReactionRemoveKey int

const (
	// ReactionRemoveByMessageID removes a reaction by the message it sat on.
	ReactionRemoveByMessageID ReactionRemoveKey = iota
	// ReactionRemoveByReactionID removes a reaction by the reaction's own id.
	ReactionRemoveByReactionID
)
