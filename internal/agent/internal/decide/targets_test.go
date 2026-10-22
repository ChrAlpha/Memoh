package decide

import (
	"context"
	"errors"
	"testing"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
)

func TestPendingRuntimeDecisionsFailsClosedOnRuntimeLookupFailure(t *testing.T) {
	sentinel := errors.New("session unavailable")
	service := newTestDecider(Deps{Queries: &agenttest.AbortedLifecycleQueries{Pending: true}, Sessions: &agenttest.Sessions{GetFn: func(context.Context, string) (sessionpkg.Thread, error) { return sessionpkg.Thread{}, sentinel }}})
	got, err := service.PendingRuntimeDecisions(t.Context(), "00000000-0000-0000-0000-000000000001")
	if !errors.Is(err, sentinel) || got != nil {
		t.Fatalf("runtime lookup error treated as native: %v %v", got, err)
	}
}
