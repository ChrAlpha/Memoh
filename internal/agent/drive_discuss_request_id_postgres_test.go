package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"github.com/felinics/memoh/internal/logger"
)

// requestIDLogs records, for each record, the request id of the context it was
// logged with.
type requestIDLogs struct {
	mu      sync.Mutex
	records []requestIDRecord
}

type requestIDRecord struct {
	message   string
	requestID string
}

func (*requestIDLogs) Enabled(context.Context, slog.Level) bool { return true }

func (l *requestIDLogs) Handle(ctx context.Context, record slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, requestIDRecord{message: record.Message, requestID: logger.RequestIDFromContext(ctx)})
	return nil
}
func (l *requestIDLogs) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *requestIDLogs) WithGroup(string) slog.Handler      { return l }

func (l *requestIDLogs) idsOf(message string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var ids []string
	for _, record := range l.records {
		if record.message == message {
			ids = append(ids, record.requestID)
		}
	}
	return ids
}

// A discuss run answers a batch of messages, not the request of the one that
// woke it, so the run and its result record carry a request id of their own:
// a different one for each run, and none of the id of whoever triggered the
// admission. Admission is reached from an input being committed, from a run
// ending and from the periodic sweep, and each of them passes through here.
func TestDiscussRunHasItsOwnRequestID(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	logs := &requestIDLogs{}
	proc := newDiscussProcessWith(t, env, []discussOption{
		discussDeps(func(d *Deps) { d.Logger = slog.New(logs) }),
	})
	alice := env.participant(t, false)

	triggers := []string{"trigger-wake", "trigger-sweep"}
	for i, trigger := range triggers {
		hw := env.write(t, passiveInput{from: alice, externalID: fmt.Sprintf("m%d", i), text: "hello"})
		proc.answerFrom(t, trigger, env, hw)
	}

	ids := logs.idsOf("agent run")
	if len(ids) != len(triggers) {
		t.Fatalf("agent run records = %v, want one per run", ids)
	}
	if ids[0] == "" || ids[1] == "" || ids[0] == ids[1] || ids[0] == triggers[0] || ids[1] == triggers[1] {
		t.Fatalf("request ids of the runs = %v, want a different id of their own for each", ids)
	}
}
