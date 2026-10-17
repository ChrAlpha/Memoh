package application

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

func TestRuntimeDecisionTerminalWithoutRecoverableSnapshotPersistsMinimalLifecycle(t *testing.T) {
	tests := []struct {
		name              string
		events            []native.StreamEvent
		wantLifecycle     string
		wantRuntimeStatus string
	}{
		{
			name:              "completed",
			events:            []native.StreamEvent{{Type: native.EventAgentEnd}},
			wantLifecycle:     lifecycle.StatusCompleted,
			wantRuntimeStatus: sessionruntime.RunStatusCompleted,
		},
		{
			name: "provider failure",
			events: []native.StreamEvent{
				{Type: native.EventError, Error: "private provider detail"},
				{Type: native.EventAgentAbort},
			},
			wantLifecycle:     lifecycle.StatusFailedProvider,
			wantRuntimeStatus: sessionruntime.RunStatusErrored,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, handle := newWaitingDecisionRuntime(t)
			lifecycles := &agenttest.RecordingLifecycleStore{}
			service := continuationService(manager)
			useLifecycleStore(service, lifecycles)
			manager.SetTerminalObserver(service.lifecycle.ReconcileTerminal)

			// The run resolves no context of its own, so the terminal write is
			// what leaves its lifecycle row.
			continueDecision(t, service, manager, decisionCommand(handle, "minimal"),
				scriptedRun(append([]native.StreamEvent{{Type: native.EventAgentStart}}, tt.events...), nil))

			rows := lifecycleRows(lifecycles)
			if len(rows) != 1 || rows[0].Status != tt.wantLifecycle {
				t.Fatalf("lifecycle rows = %#v, want one %s row", rows, tt.wantLifecycle)
			}
			var snapshot contextfrag.LifecycleSnapshot
			if err := json.Unmarshal(rows[0].Snapshot, &snapshot); err != nil {
				t.Fatalf("decode minimal lifecycle: %v", err)
			}
			if snapshot.Version != contextfrag.LifecycleSnapshotVersion || snapshot.Counts != (contextfrag.ManifestCounts{}) {
				t.Fatalf("lifecycle snapshot = %#v, want minimal version 1 snapshot", snapshot)
			}
			if bytes.Contains(rows[0].Snapshot, []byte("private provider detail")) {
				t.Fatalf("lifecycle snapshot leaked private provider detail: %s", rows[0].Snapshot)
			}
			runtimeSnapshot, err := manager.Snapshot(context.Background(), agenttest.LifecycleBotID, agenttest.LifecycleSessionID)
			if err != nil {
				t.Fatal(err)
			}
			if runtimeSnapshot.CurrentRunView == nil || runtimeSnapshot.CurrentRunView.Status != tt.wantRuntimeStatus {
				t.Fatalf("runtime terminal = %#v, want %s", runtimeSnapshot.CurrentRunView, tt.wantRuntimeStatus)
			}
		})
	}
}

func TestStaleDecisionContinuationDoesNotRecoverLifecycleFromAssistantMetadata(t *testing.T) {
	manager, handle := newWaitingDecisionRuntime(t)
	snapshot, ok := agenttest.LifecycleRunConfig().ContextLifecycle.Snapshot()
	if !ok {
		t.Fatal("test lifecycle snapshot is unavailable")
	}
	metadata, err := json.Marshal(map[string]any{
		contextfrag.MetadataContextLifecycleKey: snapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	lifecycles := &agenttest.RecordingLifecycleStore{Metadata: metadata}
	service := continuationService(manager)
	useLifecycleStore(service, lifecycles)
	stale := decisionCommand(handle, "stale")
	stale.Generation += "-stale"

	continueDecision(t, service, manager, stale, func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
		t.Fatal("stale continuation unexpectedly ran")
		return drive.RunPlan{}, nil
	})

	if rows := lifecycleRows(lifecycles); len(rows) != 0 {
		t.Fatalf("stale continuation created lifecycle rows from assistant metadata: %#v", rows)
	}
}
