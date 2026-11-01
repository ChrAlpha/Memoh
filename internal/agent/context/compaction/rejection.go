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
	// cause is the summarizer failure the refused pass reported.
	cause    error
	refusals int
	retryAt  time.Time
}

// rejections remembers, per session, a summarizer the provider refused in a
// way waiting a moment does not change: credentials, permission, quota, or a
// request it will not serve. A pass the provider refuses commits nothing, so
// the session's checkpoint stays where it was and every later Compact would
// read the same rows after it again, measuring each one, open a log row and
// be refused the same way. Until the wait passes, a Compact for that session
// fails with that refusal before it runs the hooks, reads the history or
// opens a log row. A changed summarizer (model, endpoint, credentials,
// window) is asked at once.
//
// Fold is not held back: its summarizer is the run's own model, a refusal of
// which fails the run's own requests as well, and it reads only the rows the
// run's request renders.
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

// active returns the refusal an event cfg would run fails with while the
// session's summarizer is still refused, or nil when the event may run.
func (r *rejections) active(cfg TriggerConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	last, ok := r.bySession[cfg.SessionID]
	if !ok || last.summarizer != summarizerIdentity(cfg) || !r.now().Before(last.retryAt) {
		return nil
	}
	return failed(ReasonSummarizerFailed, fmt.Errorf("%w (refusals=%d retry_at=%s): %w",
		errSummarizerRejected, last.refusals, last.retryAt.UTC().Format(time.RFC3339), last.cause))
}

// observe records how an event that ran ended. A refusal extends the wait;
// otherwise a committed pass, which the provider answered, clears it. Any
// other outcome says nothing about the provider and leaves it as it was.
func (r *rejections) observe(cfg TriggerConfig, passes int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cause := providerRefusal(err)
	if cause == nil {
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
		cause:      cause,
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

// providerRefusal is the summarizer failure err reports when the provider
// answered the call with a failure that is final, one modelretry does not
// retry, or nil for any other outcome.
func providerRefusal(err error) error {
	var event *EventError
	if !errors.As(err, &event) || event.Reason != ReasonSummarizerFailed {
		return nil
	}
	var apiErr *sdk.APIError
	if !errors.As(event.Err, &apiErr) {
		return nil
	}
	if _, transient := modelretry.Retryable(event.Err); transient {
		return nil
	}
	return event.Err
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
