package compaction

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// failingModel answers every request with one HTTP failure.
type failingModel struct {
	status int
	calls  int
}

func (f *failingModel) RoundTrip(*http.Request) (*http.Response, error) {
	f.calls++
	return &http.Response{
		StatusCode: f.status,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"stub failure"}}`)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// countingQueries counts the candidate reads of a session's history.
type countingQueries struct {
	*fakeQueries
	reads int
}

func (c *countingQueries) ListHistoryCheckpointCandidatesWithinBytes(ctx context.Context, arg sqlc.ListHistoryCheckpointCandidatesWithinBytesParams) ([]candidateRow, error) {
	c.reads++
	return c.fakeQueries.ListHistoryCheckpointCandidatesWithinBytes(ctx, arg)
}

// rejectionHarness is a service whose clock the test moves.
type rejectionHarness struct {
	svc *Service
	q   *countingQueries
	now time.Time
}

func newRejectionHarness(t *testing.T) *rejectionHarness {
	t.Helper()
	h := &rejectionHarness{
		q:   &countingQueries{fakeQueries: &fakeQueries{history: machineryCorpus(t)}},
		now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC),
	}
	h.svc = newMachineryService(h.q)
	h.svc.rejections.now = func() time.Time { return h.now }
	return h
}

// compact runs one event and reports whether it read the history or opened a
// log row.
func (h *rejectionHarness) compact(t *testing.T, cfg TriggerConfig) ([]Pass, bool, error) {
	t.Helper()
	reads, logs := h.q.reads, len(h.q.logs)
	passes, err := h.svc.Compact(context.Background(), cfg)
	return passes, h.q.reads > reads || len(h.q.logs) > logs, err
}

func refusedConfig(model *failingModel) TriggerConfig {
	cfg := machineryConfig(&stubModel{}, 450)
	cfg.HTTPClient = &http.Client{Transport: model}
	return cfg
}

func TestCompactHoldsASummarizerTheProviderRefusedUntilTheWaitPasses(t *testing.T) {
	t.Parallel()

	h := newRejectionHarness(t)
	model := &failingModel{status: http.StatusPaymentRequired}
	cfg := refusedConfig(model)

	if _, ran, err := h.compact(t, cfg); err == nil || !ran || model.calls != 1 {
		t.Fatalf("first event: err=%v ran=%v calls=%d, want a refused attempt", err, ran, model.calls)
	}

	h.now = h.now.Add(rejectionBaseWait - time.Second)
	_, ran, err := h.compact(t, cfg)
	if ran || model.calls != 1 {
		t.Fatalf("event inside the wait reached the history (ran=%v) or the provider (calls=%d)", ran, model.calls)
	}
	// The held event fails as the refused one did, so its caller still names
	// the provider's refusal.
	var apiErr *sdk.APIError
	if ReasonOf(err) != ReasonSummarizerFailed || !errors.Is(err, errSummarizerRejected) || !errors.As(err, &apiErr) || apiErr.Kind != sdk.KindQuotaExhausted {
		t.Fatalf("held event err = %v, want the provider's quota refusal", err)
	}

	h.now = h.now.Add(time.Second)
	if _, ran, err := h.compact(t, cfg); err == nil || !ran || model.calls != 2 {
		t.Fatalf("event after the wait: err=%v ran=%v calls=%d, want a second attempt", err, ran, model.calls)
	}
	// The second refusal in a row doubles the wait.
	h.now = h.now.Add(2*rejectionBaseWait - time.Second)
	if _, ran, _ := h.compact(t, cfg); ran {
		t.Fatal("the second refusal must double the wait")
	}
	h.now = h.now.Add(time.Second)
	if _, ran, _ := h.compact(t, cfg); !ran || model.calls != 3 {
		t.Fatalf("event after the doubled wait: ran=%v calls=%d, want a third attempt", ran, model.calls)
	}
}

func TestCompactHoldsARequestTheProviderWillNotServe(t *testing.T) {
	t.Parallel()

	h := newRejectionHarness(t)
	model := &failingModel{status: http.StatusBadRequest}
	cfg := refusedConfig(model)
	if _, _, err := h.compact(t, cfg); err == nil {
		t.Fatal("first event must be refused")
	}
	if _, ran, err := h.compact(t, cfg); ran || model.calls != 1 || !errors.Is(err, errSummarizerRejected) {
		t.Fatalf("a final refusal must hold the next event: ran=%v calls=%d err=%v", ran, model.calls, err)
	}
}

func TestCompactAsksAChangedSummarizerAtOnce(t *testing.T) {
	t.Parallel()

	h := newRejectionHarness(t)
	model := &failingModel{status: http.StatusPaymentRequired}
	cfg := refusedConfig(model)
	if _, _, err := h.compact(t, cfg); err == nil {
		t.Fatal("first event must be refused")
	}

	rotated := cfg
	rotated.APIKey = "rotated"
	if _, ran, _ := h.compact(t, rotated); !ran || model.calls != 2 {
		t.Fatalf("new credentials must be asked at once (ran=%v calls=%d)", ran, model.calls)
	}

	switched := cfg
	switched.ModelRecordID = "another-model"
	switched.HTTPClient = &http.Client{Transport: &stubModel{summary: "SUMMARY"}}
	if passes, _, err := h.compact(t, switched); err != nil || len(passes) == 0 {
		t.Fatalf("another model must be asked at once: passes=%d err=%v", len(passes), err)
	}
}

func TestCompactAnswerClearsTheRefusal(t *testing.T) {
	t.Parallel()

	h := newRejectionHarness(t)
	cfg := refusedConfig(&failingModel{status: http.StatusPaymentRequired})
	_, _, _ = h.compact(t, cfg)

	h.now = h.now.Add(rejectionBaseWait)
	cfg.HTTPClient = &http.Client{Transport: &stubModel{summary: "SUMMARY"}}
	if passes, _, err := h.compact(t, cfg); err != nil || len(passes) == 0 {
		t.Fatalf("event after the wait: passes=%d err=%v", len(passes), err)
	}
	if err := h.svc.rejections.active(cfg); err != nil {
		t.Fatalf("a committed pass must clear the refusal: %v", err)
	}
}

func TestCompactKeepsAskingAfterATransientFailure(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		h := newRejectionHarness(t)
		model := &failingModel{status: status}
		cfg := refusedConfig(model)
		if _, _, err := h.compact(t, cfg); err == nil {
			t.Fatalf("status %d: the event must fail", status)
		}
		calls := model.calls
		if _, ran, _ := h.compact(t, cfg); !ran || model.calls == calls {
			t.Fatalf("status %d: a transient failure must not hold the next event back (ran=%v calls=%d)", status, ran, model.calls)
		}
	}
}

func TestRejectionWaitDoublesUpToTheCap(t *testing.T) {
	t.Parallel()

	want := []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, wait := range want {
		if got := rejectionWait(i + 1); got != wait {
			t.Fatalf("rejectionWait(%d) = %s, want %s", i+1, got, wait)
		}
	}
}
