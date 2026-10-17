package application

import (
	"context"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
)

type nilQueries struct{ dbstore.Queries }

type queueTestFence struct{}

func (queueTestFence) Activate(context.Context, string, string, int64) error { return nil }

// newDeferredSteerTestService builds a Service whose queue step transaction
// can run without PostgreSQL: steps carry no messages, so history persistence
// is skipped, and queue state lives in a memory backend with one active run.
func newDeferredSteerTestService(t *testing.T, backends ...sessionruntime.Backend) (*Service, sessionruntime.RunHandle) {
	t.Helper()
	var backend sessionruntime.Backend = sessionruntime.NewMemoryBackend()
	if len(backends) > 0 {
		backend = backends[0]
	}
	manager := sessionruntime.NewManager(backend, sessionruntime.Options{OwnerID: "owner-1", Ledger: sessionledger.New(), Fence: queueTestFence{}})
	t.Cleanup(func() { _ = manager.Close() })
	admitted, err := manager.Admit(context.Background(), sessionruntime.AdmitInput{
		BotID: "bot", SessionID: "session", InvocationID: "initial", Payload: []byte(`{}`),
		Execution: sessionruntime.Execution{Admission: func(context.Context, sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error) {
			return sessionruntime.RunAdmissionView{}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle := admitted.Handle
	if err := manager.EnableSteer(context.Background(), handle); err != nil {
		t.Fatal(err)
	}
	service := wireComponents(&Service{
		sessionManager: manager,
		messageService: &agenttest.RecordingStepPersister{RecordingMessages: &agenttest.RecordingMessages{}},
		queries:        nilQueries{},
		queue:          &memoryQueuedInputs{},
	})
	return service, handle
}
