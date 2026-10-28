package models

import (
	"context"
	"io"
	"net/http"
)

type activityKey struct{}

// withActivity has every provider response made under ctx report to observe
// as it arrives: when its headers come and whenever its body yields bytes.
// Bytes the SDK drops on the way to a stream part, such as an Anthropic ping or
// an SSE comment a relay sends to keep the line open, still count.
func withActivity(ctx context.Context, observe func()) context.Context {
	return context.WithValue(ctx, activityKey{}, observe)
}

// observeActivity returns client with a transport that reports provider
// responses to the observer their request's context carries.
func observeActivity(client *http.Client) *http.Client {
	if _, ok := client.Transport.(activityTransport); ok {
		return client
	}
	observed := *client
	observed.Transport = activityTransport{base: client.Transport}
	return &observed
}

type activityTransport struct {
	base http.RoundTripper
}

func (t activityTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	observe, _ := req.Context().Value(activityKey{}).(func())
	if err != nil || observe == nil {
		return resp, err
	}
	observe()
	// A switched protocol's body is the connection itself.
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.Body != nil {
		resp.Body = activityBody{ReadCloser: resp.Body, observe: observe}
	}
	return resp, nil
}

type activityBody struct {
	io.ReadCloser
	observe func()
}

func (b activityBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.observe()
	}
	return n, err
}
