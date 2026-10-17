package runconfig

import (
	"net/http"
	"testing"
)

// The model client bounds connection setup only. Silence inside a stream is
// the run's idle watchdog's to judge, so a whole-request or response-header
// deadline here would pre-empt it.
func TestStreamHTTPClientHasNoRequestDeadline(t *testing.T) {
	client := newBuilder().httpClient
	if client.Timeout != 0 {
		t.Fatalf("stream client timeout = %v, want no whole-request deadline", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("stream transport = %T, want *http.Transport", client.Transport)
	}
	if transport.ResponseHeaderTimeout != 0 {
		t.Fatalf("stream response header timeout = %v, want application watchdog ownership", transport.ResponseHeaderTimeout)
	}
}
