package subagent

import (
	"context"
	"testing"

	"github.com/google/uuid"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

func TestObserverRequiresRuntimeAndHandle(t *testing.T) {
	host := New(Deps{})
	if got := host.Observer(runContext(uuid.NewString(), uuid.NewString(), 5)); got != nil {
		t.Fatal("observer enabled without a session runtime")
	}
	withRuntime := New(Deps{Events: &sessionruntime.Manager{}})
	if got := withRuntime.Observer(context.Background()); got != nil {
		t.Fatal("observer enabled without an admitted run handle")
	}
}
