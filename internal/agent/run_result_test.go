package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	"github.com/felinics/memoh/internal/errlog"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
)

// A run through a real session runtime manager writes one result record when
// it succeeds.
func TestRunResultOneRecordForASuccessfulRun(t *testing.T) {
	logger, logs := agenttest.CaptureLogs()
	fixture, _ := newScriptedFailureFixture(t, nil, logger)

	got := runNativeTurn(t, fixture)

	if got.ledger[0] != string(ledger.StateCompleted) {
		t.Fatalf("session_runs state = %q, want completed", got.ledger[0])
	}
	records := waitRunResults(t, logs, 1)
	if records[0].Level != slog.LevelInfo || agenttest.RecordAttrs(records[0])["state"] != "completed" {
		t.Fatalf("result record = %v %v, want INFO completed", records[0].Level, agenttest.RecordAttrs(records[0]))
	}
}

// A run whose stream reports several failures (a failed attempt, its retry,
// the final failure) writes one ERROR, its result record. The failure events
// themselves are not logged at ERROR.
func TestRunResultOneErrorForARunWithSeveralFailureEvents(t *testing.T) {
	logger, logs := agenttest.CaptureLogs()
	fixture, _ := newScriptedFailureFixture(t, nil, logger, http.StatusServiceUnavailable, http.StatusBadRequest)

	got := runNativeTurn(t, fixture)

	assertOrdered(t, got.types, "retry", "error", "agent_abort")
	records := waitRunResults(t, logs, 1)
	attrs := agenttest.RecordAttrs(records[0])
	if attrs["state"] != "failed" || attrs["error_code"] != got.ledger[1] || attrs["fault"] == "" {
		t.Fatalf("result record = %v, want the failed run with its code and fault", attrs)
	}
	if strings.Contains(attrs["error"], "without its owner's cause") {
		t.Fatalf("result record error = %q, want the owner's cause", attrs["error"])
	}
	if errorsLogged := logs.AtLevel(slog.LevelError); len(errorsLogged) != 1 || errorsLogged[0].Message != "agent run" {
		messages := make([]string, 0, len(errorsLogged))
		for _, record := range errorsLogged {
			messages = append(messages, record.Message)
		}
		t.Fatalf("ERROR records = %q, want only the run's result record", messages)
	}
	// The consumer of the turn (an inbound message, the turn RPC) finishes
	// its own unit with the error the handle gave it; that record is a WARN
	// pointing at the run's ERROR, not a second ERROR.
	if len(got.errs) != 1 {
		t.Fatalf("handle errors = %v, want the run's failure", got.errs)
	}
	if outer := errlog.Finish(context.Background(), "channel.inbound", got.errs[0], errlog.Options{}); outer.Level != slog.LevelWarn {
		t.Fatalf("consumer record level = %v, want WARN", outer.Level)
	}
}

// When the terminal event's proposal write fails, the run ends through the
// owner's finish instead; the run still has one result record.
func TestRunResultOneRecordWhenTheProposalIsDeferred(t *testing.T) {
	logger, logs := agenttest.CaptureLogs()
	runs := &proposalRecordingLedger{Store: sessionledger.New(), failFirst: true}
	fixture, _ := newScriptedFailureFixture(t, runs, logger, http.StatusServiceUnavailable, http.StatusBadRequest)

	got := runNativeTurn(t, fixture)

	if got.ledger[0] != string(ledger.StateFailed) {
		t.Fatalf("session_runs state = %q, want failed", got.ledger[0])
	}
	waitRunResults(t, logs, 1)
}

// waitRunResults waits for the terminal observer, which runs after the turn's
// handle closes, and then requires exactly want result records.
func waitRunResults(t *testing.T, logs *agenttest.CapturedLogs, want int) []slog.Record {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(logs.RunResults()) < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// Give a duplicate observation the same chance to arrive.
	time.Sleep(50 * time.Millisecond)
	records := logs.RunResults()
	if len(records) != want {
		attrs := make([]map[string]string, 0, len(records))
		for _, record := range records {
			attrs = append(attrs, agenttest.RecordAttrs(record))
		}
		encoded, _ := json.Marshal(attrs)
		t.Fatalf("result records = %s, want %d", encoded, want)
	}
	return records
}

// A Web run that fails reports the failure on its turn span, the same failure
// the run's result record reports.
func TestTurnSpanReportsAFailureTheWebSocketStreamDelivered(t *testing.T) {
	recorder := recordTurnSpans(t)
	fixture, _ := newScriptedFailureFixture(t, nil, nil, http.StatusServiceUnavailable, http.StatusBadRequest)

	_, err := streamWebSend(context.Background(), t, fixture.service, runreq.ChatRequest{
		BotID:                agenttest.LifecycleBotID,
		ChatID:               agenttest.LifecycleBotID,
		ThreadID:             agenttest.LifecycleSessionID,
		Query:                directLifecyclePrompt,
		UserMessagePersisted: true,
	})
	if err == nil {
		t.Fatal("Web send succeeded, want a failed run")
	}
	span := turnSpan(t, recorder)
	if got := spanAttr(span, "agent.turn.outcome").AsString(); got != "errored" {
		t.Errorf("outcome = %q, want errored", got)
	}
	if span.Status().Code != codes.Error {
		t.Error("the turn span of a failed run is not an error")
	}
}
