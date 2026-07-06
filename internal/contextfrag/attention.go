package contextfrag

import (
	"strings"

	"github.com/memohai/memoh/internal/conversation"
)

// AttentionDerivation is the shared IM attention core behind the chat flow
// scope and the discuss per-segment scope.
type AttentionDerivation struct {
	ConversationType string
	MentionsBot      bool
	RepliesToBot     bool
	// UnknownTypeKind buckets an empty conversation type. Chat uses
	// conversation.KindDirect: the current turn carries the prior that the
	// user is addressing the bot. Discuss history segments use
	// conversation.KindGroup: replayed group traffic has no such prior.
	// Unrecognized non-empty types keep the passive fallback on both sides.
	UnknownTypeKind string
	// Leading reasons precede mention/reply and Trailing reasons follow
	// them; both gate the passive fallback. The chat flow layers its
	// schedule/heartbeat/command branches through these.
	Leading  []AttentionReason
	Trailing []AttentionReason
}

// DeriveAttention derives the attention reasons for one inbound message.
func DeriveAttention(d AttentionDerivation) []AttentionReason {
	var reasons []AttentionReason
	add := func(reason AttentionReason) {
		for _, existing := range reasons {
			if existing == reason {
				return
			}
		}
		reasons = append(reasons, reason)
	}
	for _, reason := range d.Leading {
		add(reason)
	}
	if d.MentionsBot {
		add(AttentionMention)
	}
	if d.RepliesToBot {
		add(AttentionReply)
	}
	for _, reason := range d.Trailing {
		add(reason)
	}
	kind := strings.ToLower(strings.TrimSpace(d.ConversationType))
	if kind == "" {
		kind = d.UnknownTypeKind
	}
	switch kind {
	case conversation.KindDirect, "private":
		add(AttentionDirect)
	case conversation.KindGroup, conversation.KindThread:
		if len(reasons) == 0 {
			add(AttentionPassive)
		}
	}
	if len(reasons) == 0 {
		add(AttentionPassive)
	}
	return reasons
}
