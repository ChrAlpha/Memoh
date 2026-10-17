package application

import (
	"net/http"
	"testing"
	"time"
)

func TestServiceStreamHTTPClientHasNoRequestDeadline(t *testing.T) {
	service := newTestService(t, Deps{ClockLocation: time.UTC})
	if service.streamHTTPClient.Timeout != 0 {
		t.Fatalf("stream client timeout = %v, want no whole-request deadline", service.streamHTTPClient.Timeout)
	}
	streamTransport, ok := service.streamHTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("stream transport = %T, want *http.Transport", service.streamHTTPClient.Transport)
	}
	if streamTransport.ResponseHeaderTimeout != 0 {
		t.Fatalf("stream response header timeout = %v, want application watchdog ownership", streamTransport.ResponseHeaderTimeout)
	}
}
