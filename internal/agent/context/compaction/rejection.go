package compaction

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/models/modelretry"
)

// A summarizer the provider refused is not asked again for the same session
// until the wait has passed: first rejectionBaseWait, doubling with each
// refusal in a row up to rejectionMaxWait.
const (
	rejectionBaseWait = 2 * time.Minute
	rejectionMaxWait  = 30 * time.Minute
)

// errSummarizerRejected marks a compaction event skipped because the
// session's summarizer refused the last request.
var errSummarizerRejected = errors.New("compaction: summarizer refused the last request")

// rejection is the provider's last refusal of a session's summarizer.
type rejection struct {
	summarizer [sha256.Size]byte
	err        error
	refusals   int
	retryAt    time.Time
}

// rejections remembers, per session, a summarizer the provider refused in a
// way waiting a moment does not change: credentials, permission, quota, or a
// request it will not serve. Until the wait passes, an event for that session
// fails with the same reason before it reads the backlog, claims rows or
// writes a log row, as the event it would have run fails. A changed
// summarizer (model, endpoint, credentials, window) is asked at once.
//
// The memory is per process. It saves work and decides nothing durable: a
// restart, or another server, pays at most one more refusal per wait.
type rejections struct {
	mu        sync.Mutex
	now       func() time.Time
	bySession map[string]rejection
}

func newRejections() *rejections {
	return &rejections{now: time.Now, bySession: make(map[string]rejection)}
}

// active returns an error for an event cfg would run while the session's
// summarizer is still refused, or nil when the event may run.
func (r *rejections) active(cfg TriggerConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	last, ok := r.bySession[cfg.SessionID]
	if !ok || last.summarizer != summarizerIdentity(cfg) || !r.now().Before(last.retryAt) {
		return nil
	}
	return failed(ReasonSummarizerFailed, fmt.Errorf("%w (refusals=%d retry_at=%s): %w",
		errSummarizerRejected, last.refusals, last.retryAt.UTC().Format(time.RFC3339), last.err))
}

// observe records how an event that ran ended. A refusal extends the wait;
// otherwise a committed pass, which the provider answered, clears it. Any
// other outcome says nothing about the provider and leaves it as it was.
func (r *rejections) observe(cfg TriggerConfig, passes int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !refusedByProvider(err) {
		if passes > 0 {
			delete(r.bySession, cfg.SessionID)
		}
		return
	}
	now := r.now()
	r.forgetStale(now)
	summarizer := summarizerIdentity(cfg)
	refusals := 1
	if last, ok := r.bySession[cfg.SessionID]; ok && last.summarizer == summarizer {
		refusals = last.refusals + 1
	}
	r.bySession[cfg.SessionID] = rejection{
		summarizer: summarizer,
		err:        err,
		refusals:   refusals,
		retryAt:    now.Add(rejectionWait(refusals)),
	}
}

// forgetStale drops refusals whose wait passed long enough ago that the
// session stopped asking: its next refusal starts over from the base wait.
func (r *rejections) forgetStale(now time.Time) {
	for session, last := range r.bySession {
		if now.Sub(last.retryAt) > rejectionMaxWait {
			delete(r.bySession, session)
		}
	}
}

func rejectionWait(refusals int) time.Duration {
	wait := rejectionBaseWait
	for i := 1; i < refusals && wait < rejectionMaxWait; i++ {
		wait *= 2
	}
	return min(wait, rejectionMaxWait)
}

// refusedByProvider reports a summarizer call the provider answered with a
// failure that is final: one modelretry does not retry.
func refusedByProvider(err error) bool {
	if ReasonOf(err) != ReasonSummarizerFailed {
		return false
	}
	var apiErr *sdk.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	_, transient := modelretry.Retryable(err)
	return !transient
}

// summarizerIdentity digests what the provider's answer depends on besides
// the request, so a change to any of it asks again. Credentials are only held
// as part of the digest.
func summarizerIdentity(cfg TriggerConfig) [sha256.Size]byte {
	h := sha256.New()
	for _, field := range []string{
		cfg.ModelRecordID, cfg.ModelID, cfg.ClientType, cfg.BaseURL, cfg.ChatCompletionsCompat,
		cfg.APIKey, cfg.CodexAccountID,
		strconv.Itoa(cfg.SummaryWindowTokens), strconv.Itoa(cfg.MaxOutputTokens),
	} {
		h.Write([]byte(field))
		h.Write([]byte{0})
	}
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	return sum
}
