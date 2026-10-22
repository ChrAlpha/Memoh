package pending_test

import (
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/pending"
)

// The sweep runs every minute and looks half an hour back.
func TestDiscussSweepConstants(t *testing.T) {
	if pending.DiscussSweepInterval != time.Minute {
		t.Fatalf("pending.DiscussSweepInterval = %v, want 1m", pending.DiscussSweepInterval)
	}
	if pending.DiscussSweepWindow <= pending.DiscussSweepInterval {
		t.Fatalf("pending.DiscussSweepWindow = %v, want longer than the interval", pending.DiscussSweepWindow)
	}
}
