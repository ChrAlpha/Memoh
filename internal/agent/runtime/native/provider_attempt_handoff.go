package native

import (
	"errors"
	"sync"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
)

var errProviderAttemptNotPrepared = errors.New("provider attempt was not prepared")

type preparedProviderAttempt struct {
	snapshot          contextfrag.StepSnapshot
	systemPrepended   bool
	reselectionDetail string
}

// providerAttemptHandoff publishes successful attempt state at the last
// Memoh-owned boundary before invoking the provider. Preparation may run well
// before that boundary, so it stages content-light metadata here instead of
// advancing hash, fork, retry, or durable-input state early.
type providerAttemptHandoff struct {
	mu      sync.Mutex
	cfg     RunConfig
	pending *preparedProviderAttempt
}

func newProviderAttemptHandoff(cfg RunConfig) *providerAttemptHandoff {
	return &providerAttemptHandoff{cfg: cfg}
}

func (h *providerAttemptHandoff) stage(snapshot contextfrag.StepSnapshot, systemPrepended bool, reselectionDetail string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.pending = &preparedProviderAttempt{
		snapshot:          snapshot,
		systemPrepended:   systemPrepended,
		reselectionDetail: reselectionDetail,
	}
	h.mu.Unlock()
}

func (h *providerAttemptHandoff) reject() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.pending = nil
	h.mu.Unlock()
	h.cfg.preparedStepMessages.reconcileLast(nil)
}

func (h *providerAttemptHandoff) publish(params sdk.GenerateParams) error {
	if h == nil {
		return errProviderAttemptNotPrepared
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending == nil {
		return errProviderAttemptNotPrepared
	}

	pending := *h.pending
	if h.cfg.ForkContext != nil {
		if err := h.cfg.ForkContext.Store(params.Messages); err != nil {
			h.pending = nil
			h.cfg.preparedStepMessages.reconcileLast(nil)
			return err
		}
	}

	h.cfg.preparedStepMessages.reconcileLast(params.Messages)
	hash, _ := contextfrag.ProviderPayloadHashAndBytes(params.System, params.Messages, params.Tools)
	h.cfg.providerAttemptState.store(&params, pending.snapshot.StepIndex, pending.systemPrepended)
	h.cfg.ContextMutations.SetFinalInputHash(hash)
	pending.snapshot.PostPrepareInputHash = hash
	h.cfg.ContextMutations.AppendStepSnapshot(pending.snapshot)
	if pending.reselectionDetail != "" {
		h.cfg.ContextMutations.Record(contextfrag.MutationLoopStepReselection, pending.reselectionDetail)
	}
	h.pending = nil
	return nil
}
