package compaction

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// alwaysRateLimited answers every request with a rate limit, which the
// summarizer retries and then gives up on.
type alwaysRateLimited struct{ calls int }

func (a *alwaysRateLimited) RoundTrip(*http.Request) (*http.Response, error) {
	a.calls++
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"slow down","type":"rate_limit_error"}}`)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// rejectionHarness is a service whose clock the test moves.
type rejectionHarness struct {
	svc *Service
	q   *fakeQueries
	now time.Time
}

func newRejectionHarness(t *testing.T) *rejectionHarness {
	t.Helper()
	h := &rejectionHarness{q: &fakeQueries{uncompacted: machineryCorpus(t)}, now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	h.svc = newMachineryService(h.q)
	h.svc.rejections.now = func() time.Time { return h.now }
	return h
}

// compact runs one event and reports whether it reached the database.
func (h *rejectionHarness) compact(t *testing.T, cfg TriggerConfig) ([]Pass, bool, error) {
	t.Helper()
	h.q.created = false
	passes, err := h.svc.Compact(context.Background(), cfg)
	return passes, h.q.created, err
}

func TestCompactSkipsASummarizerTheProviderRefusedUntilTheWaitPasses(t *testing.T) {
	t.Parallel()

	h := newRejectionHarness(t)
	model := &failingModel{}
	cfg := machineryConfig(&stubModel{}, 450)
	cfg.HTTPClient = &http.Client{Transport: model}

	if _, ran, err := h.compact(t, cfg); err == nil || !ran || model.calls != 1 {
		t.Fatalf("first event: err=%v ran=%v calls=%d, want a refused attempt", err, ran, model.calls)
	}

	h.now = h.now.Add(rejectionBaseWait - time.Second)
	_, ran, err := h.compact(t, cfg)
	if ran || model.calls != 1 {
		t.Fatalf("event inside the wait reached the database (ran=%v) or the provider (calls=%d)", ran, model.calls)
	}
	if ReasonOf(err) != ReasonSummarizerFailed || !errors.Is(err, errSummarizerRejected) {
		t.Fatalf("skipped event err = %v, want the summarizer refusal", err)
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
}

func TestCompactAsksAChangedSummarizerAtOnce(t *testing.T) {
	t.Parallel()

	h := newRejectionHarness(t)
	model := &failingModel{}
	cfg := machineryConfig(&stubModel{}, 450)
	cfg.HTTPClient = &http.Client{Transport: model}
	if _, _, err := h.compact(t, cfg); err == nil {
		t.Fatal("first event must be refused")
	}

	rotated := cfg
	rotated.APIKey = "rotated"
	if _, ran, _ := h.compact(t, rotated); !ran || model.calls != 2 {
		t.Fatalf("new credentials must be asked at once (ran=%v calls=%d)", ran, model.calls)
	}

	stub := &stubModel{summary: "SUMMARY"}
	switched := cfg
	switched.ModelRecordID = "another-model"
	switched.HTTPClient = &http.Client{Transport: stub}
	if passes, _, err := h.compact(t, switched); err != nil || len(passes) == 0 {
		t.Fatalf("another model must be asked at once: passes=%d err=%v", len(passes), err)
	}
}

func TestCompactAnswerClearsTheRefusal(t *testing.T) {
	t.Parallel()

	h := newRejectionHarness(t)
	model := &failingModel{}
	cfg := machineryConfig(&stubModel{}, 450)
	cfg.HTTPClient = &http.Client{Transport: model}
	_, _, _ = h.compact(t, cfg)

	h.now = h.now.Add(rejectionBaseWait)
	stub := &stubModel{summary: "SUMMARY"}
	cfg.HTTPClient = &http.Client{Transport: stub}
	if passes, _, err := h.compact(t, cfg); err != nil || len(passes) == 0 {
		t.Fatalf("event after the wait: passes=%d err=%v", len(passes), err)
	}
	if err := h.svc.rejections.active(cfg); err != nil {
		t.Fatalf("a committed pass must clear the refusal: %v", err)
	}
}

func TestCompactKeepsAskingAfterATransientFailure(t *testing.T) {
	t.Parallel()

	h := newRejectionHarness(t)
	model := &alwaysRateLimited{}
	cfg := machineryConfig(&stubModel{}, 450)
	cfg.HTTPClient = &http.Client{Transport: model}
	if _, _, err := h.compact(t, cfg); err == nil {
		t.Fatal("a rate-limited event must fail")
	}
	calls := model.calls
	if _, ran, _ := h.compact(t, cfg); !ran || model.calls == calls {
		t.Fatalf("a transient failure must not hold the next event back (ran=%v calls=%d)", ran, model.calls)
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
