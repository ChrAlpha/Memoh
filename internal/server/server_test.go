package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"testing"
)

func TestShouldSkipJWT_ChannelWebhookPaths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		path string
		want bool
	}{
		{path: "/channels/feishu/webhook/cfg-1", want: true},
		{path: "/channels/wechatoa/webhook/cfg-1", want: true},
		{path: "/channels/line/webhook/cfg-1", want: true},
		{path: "/channels/line/public/media/bot-1/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/preview.jpg", want: true},
		{path: "/channels/line/public/media/bot-1/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/metadata", want: false},
		{path: "/channels/telegram/public/media/bot-1/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/preview.jpg", want: true},
		{path: "/channels/feishu/webhook", want: false},
		{path: "/api/channels/feishu/webhook", want: false},
		{path: "/webhook-tunnel/status", want: false},
	}

	for _, tc := range cases {
		got := shouldSkipJWT(tc.path)
		if got != tc.want {
			t.Fatalf("path=%q want=%v got=%v", tc.path, tc.want, got)
		}
	}
}

func TestShouldLimitPublicRequestBody(t *testing.T) {
	t.Parallel()

	cases := []struct {
		path string
		want bool
	}{
		{path: "/channels/line/webhook/cfg-1", want: true},
		{path: "/channels/telegram/public/media/bot-1/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/preview.jpg", want: false},
		{path: "/api/fs/upload", want: false},
		{path: "/api/bots/backup/import", want: false},
	}

	for _, tc := range cases {
		got := shouldLimitPublicRequestBody(tc.path)
		if got != tc.want {
			t.Fatalf("path=%q want=%v got=%v", tc.path, tc.want, got)
		}
	}
}

func TestSafeRequestLogURIStripsPublicMediaQuery(t *testing.T) {
	t.Parallel()

	u, err := neturl.Parse("/channels/line/public/media/bot-1/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/preview.jpg?exp=123&sig=secret")
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	got := safeRequestLogURI(u, u.RequestURI())
	want := "/channels/line/public/media/bot-1/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/preview.jpg"
	if got != want {
		t.Fatalf("safeRequestLogURI = %q, want %q", got, want)
	}
}

func TestShouldSkipJWT_MCPOAuthCallbackPaths(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"/oauth/mcp/callback", "/api/oauth/mcp/callback"} {
		if !shouldSkipJWT(path) {
			t.Fatalf("path=%q should skip jwt", path)
		}
	}
}

func TestRequestLoggerSkipsHealthChecks(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := NewServer(log, ":0", "test-secret")

	req := httptest.NewRequest(http.MethodHead, "/health", nil)
	rec := httptest.NewRecorder()
	srv.echo.ServeHTTP(rec, req)

	if strings.Contains(buf.String(), "msg=request") {
		t.Fatalf("health check should not be logged as a normal request, got log %q", buf.String())
	}
}

func TestRequestLoggerKeepsNonHealthRequests(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := NewServer(log, ":0", "test-secret")

	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rec := httptest.NewRecorder()
	srv.echo.ServeHTTP(rec, req)

	if !strings.Contains(buf.String(), "msg=request") {
		t.Fatalf("non-health request should be logged, got log %q", buf.String())
	}
}
