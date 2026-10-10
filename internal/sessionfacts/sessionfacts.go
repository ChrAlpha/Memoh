// Package sessionfacts names the facts a session records about how it
// converses, each answering one question that session_mode used to answer for
// all of them. session_mode stays the preset name and display label; code that
// decides behaviour reads the fact it needs.
//
// This is a leaf package (stdlib and runtimekind only) so the session store,
// the agent runtime and the tools share one vocabulary: the store may not
// depend on the agent, and the runtime may not depend on the store.
package sessionfacts

import (
	"strings"

	"github.com/felinics/memoh/internal/runtimekind"
)

// ReplyDelivery says whether the model's ordinary text reaches the
// conversation or only what it sends explicitly does.
type ReplyDelivery string

const (
	ReplyStream   ReplyDelivery = "stream"
	ReplyExplicit ReplyDelivery = "explicit"
)

// TriggerScope says what starts a run and what one run covers: one message
// addressed to the bot, or every external event not yet covered, merged.
type TriggerScope string

const (
	TriggerAddressedMessage TriggerScope = "addressed_message"
	TriggerUncoveredEvents  TriggerScope = "uncovered_events"
)

// DecisionMode says whether a run may park to wait for a person to answer a
// tool approval or a question, or must decline it on the spot.
type DecisionMode string

const (
	DecisionPark       DecisionMode = "park"
	DecisionAutoReject DecisionMode = "auto_reject"
)

// Audience says whether the session belongs to the person who opened it or
// to a shared conversation. It is observed from the route when the session is
// created, not chosen.
type Audience string

const (
	AudiencePersonal Audience = "personal"
	AudienceShared   Audience = "shared"
)

// ExternalRuntimeDriver says who may drive the runtime owner's external
// runtime from this session: the owner alone, or every participant.
type ExternalRuntimeDriver string

const (
	RuntimeDriverOwnerOnly    ExternalRuntimeDriver = "owner_only"
	RuntimeDriverParticipants ExternalRuntimeDriver = "participants"
)

// Facts are the facts a session records apart from its audience, which comes
// from the route rather than from the preset.
type Facts struct {
	ReplyDelivery         ReplyDelivery
	TriggerScope          TriggerScope
	DecisionMode          DecisionMode
	ExternalRuntimeDriver ExternalRuntimeDriver
}

// Preset returns the facts a session of the given mode is created with.
// Schedule and subagent sessions run on behalf of a schedule or a parent
// agent, where nobody answers a decision.
func Preset(mode string) Facts {
	switch strings.TrimSpace(mode) {
	case runtimekind.ModeDiscuss:
		return Facts{
			ReplyDelivery:         ReplyExplicit,
			TriggerScope:          TriggerUncoveredEvents,
			DecisionMode:          DecisionAutoReject,
			ExternalRuntimeDriver: RuntimeDriverParticipants,
		}
	case runtimekind.ModeSchedule, runtimekind.ModeSubagent:
		return Facts{
			ReplyDelivery:         ReplyStream,
			TriggerScope:          TriggerAddressedMessage,
			DecisionMode:          DecisionAutoReject,
			ExternalRuntimeDriver: RuntimeDriverOwnerOnly,
		}
	default:
		return Facts{
			ReplyDelivery:         ReplyStream,
			TriggerScope:          TriggerAddressedMessage,
			DecisionMode:          DecisionPark,
			ExternalRuntimeDriver: RuntimeDriverOwnerOnly,
		}
	}
}

// Valid reports whether every fact holds a known value.
func (f Facts) Valid() bool {
	switch f.ReplyDelivery {
	case ReplyStream, ReplyExplicit:
	default:
		return false
	}
	switch f.TriggerScope {
	case TriggerAddressedMessage, TriggerUncoveredEvents:
	default:
		return false
	}
	switch f.DecisionMode {
	case DecisionPark, DecisionAutoReject:
	default:
		return false
	}
	switch f.ExternalRuntimeDriver {
	case RuntimeDriverOwnerOnly, RuntimeDriverParticipants:
		return true
	default:
		return false
	}
}

// Valid reports whether the audience is a known value.
func (a Audience) Valid() bool {
	return a == AudiencePersonal || a == AudienceShared
}
