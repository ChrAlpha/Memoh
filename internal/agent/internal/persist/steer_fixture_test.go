package persist

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
)

type steerTestFence struct{}

func (steerTestFence) Activate(context.Context, string, string, int64) error { return nil }

// promotedRecorder notes the inputs a run reported as taken.
type promotedRecorder struct {
	mu  sync.Mutex
	ids []string
}

func (r *promotedRecorder) ConsumePromoted(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
	return nil
}

func (r *promotedRecorder) consumedIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

// steerFixture is a Writer over one active run whose steer queue lives in a
// memory backend. Steps carry no messages unless a test adds them, so history
// persistence is skipped and no database is needed.
type steerFixture struct {
	writer   *Writer
	manager  *sessionruntime.Manager
	handle   sessionruntime.RunHandle
	messages *agenttest.RecordingStepPersister
	promoted *promotedRecorder
}

func newSteerFixture(t *testing.T, backends ...sessionruntime.Backend) steerFixture {
	t.Helper()
	var backend sessionruntime.Backend = sessionruntime.NewMemoryBackend()
	if len(backends) > 0 {
		backend = backends[0]
	}
	manager := sessionruntime.NewManager(backend, sessionruntime.Options{OwnerID: "owner-1", Ledger: sessionledger.New(), Fence: steerTestFence{}})
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
	messages := &agenttest.RecordingStepPersister{RecordingMessages: &agenttest.RecordingMessages{}}
	promoted := &promotedRecorder{}
	return steerFixture{
		writer:   newTestWriter(Deps{Messages: messages, Runtime: manager, Promoted: promoted}),
		manager:  manager,
		handle:   handle,
		messages: messages,
		promoted: promoted,
	}
}

// enqueueSteer queues text as steer input for the fixture's run.
func (f steerFixture) enqueueSteer(t *testing.T, invocationID, text string) (sessionruntime.SteerItem, error) {
	t.Helper()
	payload, err := runreq.EncodeQueueCommand(turn.StartTurnCommand{
		SchemaVersion: 1, TeamID: "team1", Mode: turn.ModeChat,
		BotID: f.handle.BotID, ChatID: f.handle.BotID, ThreadID: f.handle.SessionID,
		UserID: "user-1", SourceChannelIdentityID: "user-1", Query: text, UserVisibleText: text,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return f.manager.EnqueueSteer(context.Background(), sessionruntime.Key{BotID: f.handle.BotID, SessionID: f.handle.SessionID}, uuid.NewString(), invocationID, payload)
}
