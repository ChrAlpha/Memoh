package subagent

import (
	"context"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

func TestObserverRequiresHandle(t *testing.T) {
	host, err := New(Deps{Events: &sessionruntime.Manager{}, Steps: &agenttest.RecordingStepPersister{RecordingMessages: &agenttest.RecordingMessages{}}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := host.Observer(context.Background()); got != nil {
		t.Fatal("observer enabled without an admitted run handle")
	}
}
