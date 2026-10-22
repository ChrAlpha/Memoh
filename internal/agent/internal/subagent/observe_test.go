package subagent

import (
	"context"
	"testing"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

func TestObserverRequiresHandle(t *testing.T) {
	host := New(Deps{Events: &sessionruntime.Manager{}})
	if got := host.Observer(context.Background()); got != nil {
		t.Fatal("observer enabled without an admitted run handle")
	}
}
