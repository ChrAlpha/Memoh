package agentapi

import sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"

// SessionQueues is the user-facing view of a session's queues. Steers are
// transient and live in the configured memory or Redis runtime; follow-ups are
// rows of the session input queue.
type SessionQueues struct {
	SteerSupported bool
	Steer          []sessionruntime.SteerItem
	FollowUp       []sessionruntime.FollowUpItem
}

// QueueInput is one text input an ingress admits into a session queue. It
// carries the identity the ingress authenticated, because the continuation
// that eventually runs the input happens after the request is gone and, in a
// hosted deployment, possibly on another instance: nothing at that point can
// recover which team or user the text belonged to.
type QueueInput struct {
	TeamID       string
	BotID        string
	SessionID    string
	InvocationID string
	// UserID and SourceChannelIdentityID name the sender the way the
	// ingress's ordinary turns do. Empty values are allowed for a channel
	// that has no platform account for the sender.
	UserID                  string
	SourceChannelIdentityID string
	Text                    string
}
