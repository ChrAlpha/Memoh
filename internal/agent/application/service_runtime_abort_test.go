package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/agenttest"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

type recordingAbortRuntime struct {
	applied bool
	err     error
}

func TestSetSessionRuntimeConfiguresAbortRuntime(t *testing.T) {
	manager := &sessionruntime.Manager{}
	service := wireComponents(&Service{})

	service.bindSessionRuntime(manager)

	if service.abortRuntime != manager {
		t.Fatalf("abort runtime = %T, want injected session manager", service.abortRuntime)
	}
}

func (r *recordingAbortRuntime) AbortControl(
	context.Context,
	string,
	string,
	string,
	string,
) (bool, error) {
	return r.applied, r.err
}

func TestAbortRuntimeRunReconcilesAssistantLifecycleWithoutChangingAck(t *testing.T) {
	snapshot := agenttest.LifecycleRunConfig().ContextLifecycle
	want, ok := snapshot.Snapshot()
	if !ok {
		t.Fatal("test lifecycle snapshot is unavailable")
	}
	metadata, err := json.Marshal(map[string]any{contextfrag.MetadataContextLifecycleKey: want})
	if err != nil {
		t.Fatal(err)
	}
	const assistantMessageID = "66666666-6666-4666-8666-666666666666"
	want.AssistantMessageID = assistantMessageID
	wantRaw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name             string
		upsertErr        error
		wantFailureCount uint64
	}{
		{name: "writes recovered snapshot"},
		{name: "store failure leaves acknowledgement", upsertErr: errors.New("database unavailable"), wantFailureCount: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queries := agenttest.NewAbortedLifecycleQueries(t)
			queries.AssistantID = flowTestUUID(assistantMessageID)
			queries.Metadata = metadata
			queries.UpsertErr = tt.upsertErr
			service := newAbortService(queries)

			applied, err := service.AbortRuntimeRun(
				context.Background(),
				agenttest.LifecycleBotID,
				agenttest.LifecycleSessionID,
				agenttest.LifecycleRunID,
				"abort-control-1",
			)
			if err != nil || !applied {
				t.Fatalf("AbortRuntimeRun() = (%t, %v), want (true, nil)", applied, err)
			}
			queries.WaitForUpsert(t)
			upserts := queries.RecordedUpserts()
			if len(upserts) != 1 {
				t.Fatalf("aborted upserts = %#v, want one recovered snapshot", upserts)
			}
			var gotFields, wantFields map[string]any
			if err := json.Unmarshal(upserts[0].Snapshot, &gotFields); err != nil {
				t.Fatalf("decode recovered snapshot: %v", err)
			}
			if err := json.Unmarshal(wantRaw, &wantFields); err != nil {
				t.Fatalf("decode expected snapshot: %v", err)
			}
			if !reflect.DeepEqual(gotFields, wantFields) {
				t.Fatalf("recovered snapshot = %s, want %s", upserts[0].Snapshot, wantRaw)
			}
			waitForLifecycleFailureCount(t, service, tt.wantFailureCount)
		})
	}
}

func TestAbortRuntimeRunMalformedAssistantLifecycleFallsBackToMinimal(t *testing.T) {
	queries := agenttest.NewAbortedLifecycleQueries(t)
	queries.AssistantID = flowTestUUID("77777777-7777-4777-8777-777777777777")
	queries.Metadata = []byte(`{"context_lifecycle":"invalid"}`)
	service := newAbortService(queries)

	applied, err := service.AbortRuntimeRun(
		context.Background(),
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
		agenttest.LifecycleRunID,
		"abort-malformed-metadata",
	)
	if err != nil || !applied {
		t.Fatalf("AbortRuntimeRun() = (%t, %v), want (true, nil)", applied, err)
	}
	queries.WaitForUpsert(t)
	upserts := queries.RecordedUpserts()
	minimal, marshalErr := json.Marshal(lifecycle.MinimalSnapshot())
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if len(upserts) != 1 || !bytes.Equal(upserts[0].Snapshot, minimal) {
		t.Fatalf("aborted upserts = %#v, want minimal fallback %s", upserts, minimal)
	}
	if got := service.lifecycle.PersistenceErrors(); got != 0 {
		t.Fatalf("persistence failure count = %d, want 0 for non-recoverable metadata", got)
	}
}

func TestAbortRuntimeRunFallsBackToMinimalAfterPendingDecisionGrace(t *testing.T) {
	queries := agenttest.NewAbortedLifecycleQueries(t)
	queries.Pending = true
	queries.PendingUntil = 2
	service := newAbortService(queries)

	applied, err := service.AbortRuntimeRun(
		context.Background(),
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
		agenttest.LifecycleRunID,
		"abort-before-context",
	)
	if err != nil || !applied {
		t.Fatalf("AbortRuntimeRun() = (%t, %v), want (true, nil)", applied, err)
	}
	queries.WaitForUpsert(t)
	upserts := queries.RecordedUpserts()
	if len(upserts) != 1 {
		t.Fatalf("aborted upserts = %d, want 1", len(upserts))
	}
	minimal, err := json.Marshal(lifecycle.MinimalSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(upserts[0].Snapshot, minimal) {
		t.Fatalf("fallback snapshot = %s, want %s", upserts[0].Snapshot, minimal)
	}
	pendingReads := queries.PendingReads()
	if pendingReads < 3 {
		t.Fatalf("pending decision reads = %d, want recheck after pending cleared", pendingReads)
	}
}

func TestAbortRuntimeRunPrefersExistingAuthoritativeSnapshot(t *testing.T) {
	queries := agenttest.NewAbortedLifecycleQueries(t)
	queries.Existing = &sqlc.GetContextLifecycleByRunIDRow{Snapshot: []byte(`{"version":7}`)}
	service := newAbortService(queries)

	_, err := service.AbortRuntimeRun(
		context.Background(),
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
		agenttest.LifecycleRunID,
		"abort-existing",
	)
	if err != nil {
		t.Fatalf("AbortRuntimeRun() error = %v", err)
	}
	queries.WaitForUpsert(t)
	upserts := queries.RecordedUpserts()
	if len(upserts) != 1 || string(upserts[0].Snapshot) != `{"version":7}` {
		t.Fatalf("aborted upserts = %#v, want existing authoritative snapshot", upserts)
	}
}

func waitForLifecycleFailureCount(t *testing.T, service *Service, want uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for service.lifecycle.PersistenceErrors() != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := service.lifecycle.PersistenceErrors(); got != want {
		t.Fatalf("persistence failure count = %d, want %d", got, want)
	}
}

func newAbortService(queries *agenttest.AbortedLifecycleQueries) *Service {
	service := wireComponents(&Service{queries: queries, abortRuntime: &recordingAbortRuntime{applied: true}})
	useLifecycleStore(service, queries)
	return service
}
