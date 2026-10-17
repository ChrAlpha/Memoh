package application

import (
	"net/http"
	"testing"
	"time"
)

func TestCompactionClientOutlivesColdSummarizerTTFB(t *testing.T) {
	t.Parallel()

	svc := newTestService(t, Deps{})

	if svc.compactionHTTPClient == nil {
		t.Fatal("compaction HTTP client not wired")
	}
	transport, ok := svc.compactionHTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("compaction client transport = %T, want *http.Transport", svc.compactionHTTPClient.Transport)
	}
	if transport.ResponseHeaderTimeout < 2*time.Minute {
		t.Fatalf("compaction ResponseHeaderTimeout = %v, want >= 2m (cold summarizer TTFB measured 60-93s)", transport.ResponseHeaderTimeout)
	}

	if svc.compactionHTTPClient == svc.streamHTTPClient || transport == svc.streamHTTPClient.Transport {
		t.Fatal("compaction must not share the interactive stream client or its transport")
	}
}
