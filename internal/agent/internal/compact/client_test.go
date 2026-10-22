package compact

import (
	"net/http"
	"testing"
	"time"
)

func TestCompactionClientOutlivesColdSummarizerTTFB(t *testing.T) {
	t.Parallel()

	c := New(Deps{})

	if c.httpClient == nil {
		t.Fatal("compaction HTTP client not wired")
	}
	transport, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("compaction client transport = %T, want *http.Transport", c.httpClient.Transport)
	}
	if transport.ResponseHeaderTimeout < 2*time.Minute {
		t.Fatalf("compaction ResponseHeaderTimeout = %v, want >= 2m (cold summarizer TTFB measured 60-93s)", transport.ResponseHeaderTimeout)
	}
}
