package resume

import (
	"context"
	"errors"
	"log/slog"
	"testing"
)

// Without a readiness check a continuation is claimed whatever the workspace is
// doing; with one, its refusal is the continuation's.
func TestReadinessDefaultsToPassing(t *testing.T) {
	w := New(Deps{Logger: slog.New(slog.DiscardHandler)})
	if err := w.probeReady(t.Context(), "bot", "target"); err != nil {
		t.Fatalf("probeReady() = %v, want the check to pass", err)
	}

	refused := errors.New("bridge not ready")
	w = New(Deps{Logger: slog.New(slog.DiscardHandler), Ready: func(context.Context, string, string) error { return refused }})
	if err := w.probeReady(t.Context(), "bot", "target"); !errors.Is(err, refused) {
		t.Fatalf("probeReady() = %v, want the check's refusal", err)
	}
}
