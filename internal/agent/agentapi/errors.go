package agentapi

import "errors"

// ErrQueueInputIncomplete reports an ingress that admitted queue input without
// the identity the continuation needs. It is a wiring error at the ingress,
// not a rejected user request, so handlers surface it as unavailable.
var ErrQueueInputIncomplete = errors.New("queue: input is missing team, bot, or session identity")

// ErrQueueItemNotEditable reports an edit by anyone other than the sender the
// item recorded. A queued command runs as that sender, with their attribution,
// credentials, and memory, so rewriting its text is theirs alone; others with
// queue access can still cancel or reorder it.
var ErrQueueItemNotEditable = errors.New("queue: only the sender can edit a queued item")

// ErrDirectModelUnavailable reports a direct runtime model preference that the
// runtime's catalog cannot satisfy.
var ErrDirectModelUnavailable = errors.New("direct runtime model unavailable")

// ErrExternalForkUnsupported reports a fork request against an external
// runtime that cannot fork.
var ErrExternalForkUnsupported = errors.New("runtime does not support forking")

// ErrExternalForkAnchorMissing reports a fork anchored at a turn that
// recorded no runtime turn id, so the runtime-side cut cannot match the
// visible history.
var ErrExternalForkAnchorMissing = errors.New("fork source turn has no runtime turn anchor")

// ErrModelPreferenceInvalid marks a model reference the caller can fix by
// picking another model: missing, unknown, disabled, or not a chat model.
// Store and settings failures never carry it.
var ErrModelPreferenceInvalid = errors.New("invalid model preference")

// ErrModelPreferenceRevisionInvalid means expected_model_preference_revision
// is not a UUID.
var ErrModelPreferenceRevisionInvalid = errors.New("invalid model preference revision")

// ErrACPPreferenceUnsupported means the session runs on ACP, whose model
// choice goes through the ACP runtime rather than the session preference.
var ErrACPPreferenceUnsupported = errors.New("ACP preferences must be changed through the ACP runtime")

// ErrModelPreferenceConflict means a picker read predates a newer write.
var ErrModelPreferenceConflict = errors.New("session model preference changed")
