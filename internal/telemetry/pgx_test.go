package telemetry_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/felinics/memoh/internal/telemetry"
)

// PostgreSQL writes the offending values into its error message, so recording
// the error verbatim would put the contents of a row in the trace — the same
// data the query arguments were withheld to protect.
func TestPgxTracerClassifiesAnErrorWithoutQuotingIt(t *testing.T) {
	recorder := recordSpans(t)
	tracer := telemetry.NewPgxTracer(&pgx.ConnConfig{})

	ctx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{
		SQL:  "INSERT INTO users (email) VALUES ($1)",
		Args: []any{"someone@example.test"},
	})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: &pgconn.PgError{
		Code:     "23505",
		Message:  "duplicate key value violates unique constraint \"users_email_key\"",
		Detail:   "Key (email)=(someone@example.test) already exists.",
		Severity: "ERROR",
	}})

	span := onlySpan(t, recorder)
	if span.Status().Code != codes.Error {
		t.Error("a failed query was not marked as an error")
	}
	if got := attrs(span)["db.response.status_code"].AsString(); got != "23505" {
		t.Errorf("db.response.status_code = %q, want the SQLSTATE", got)
	}
	for key, value := range attrs(span) {
		if strings.Contains(value.String(), "someone@example.test") {
			t.Errorf("attribute %s carries the row's value: %s", key, value.String())
		}
	}
	for _, event := range span.Events() {
		for _, kv := range event.Attributes {
			if strings.Contains(kv.Value.String(), "someone@example.test") {
				t.Errorf("event %s carries the row's value: %s", event.Name, kv.Value.String())
			}
		}
	}
	if got := attrs(span)["db.query.text"].AsString(); got == "" {
		t.Error("the statement text is missing; without it a slow query span says nothing")
	}
}

// An error with no SQLSTATE still has to be distinguishable — a dial failure,
// a closed pool and a cancelled query are different problems.
func TestPgxTracerKeepsAClassForErrorsWithoutASQLSTATE(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"cancelled", context.Canceled, "context.Canceled"},
		{"deadline", context.DeadlineExceeded, "context.DeadlineExceeded"},
		{"other", errors.New("dial tcp: connection refused"), "*errors.errorString"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := recordSpans(t)
			tracer := telemetry.NewPgxTracer(&pgx.ConnConfig{})
			ctx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
			tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: tc.err})

			span := onlySpan(t, recorder)
			if got := attrs(span)["error.type"].AsString(); got != tc.want {
				t.Errorf("error.type = %q, want %q", got, tc.want)
			}
		})
	}
}

func onlySpan(t *testing.T, recorder *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()
	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("want one span, got %d", len(ended))
	}
	if got := ended[0].Name(); got != "postgresql.query" {
		t.Fatalf("span name = %q, want postgresql.query", got)
	}
	return ended[0]
}

// Every acquire is in the wait-time histogram, under the same pool name the
// pgxpool.* metrics use. Only one that waited, or failed, gets a span, and
// that span covers the wait itself.
func TestPgxTracerRecordsEveryAcquireWaitAndSpansTheSlowOnes(t *testing.T) {
	recorder := recordSpans(t)
	reader := sdkmetric.NewManualReader()
	provider := telemetry.MeterProviderForTest(reader)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() { otel.SetMeterProvider(previous) })

	cfg, err := pgx.ParseConfig("postgres://memoh@db.internal:5432/memoh")
	if err != nil {
		t.Fatal(err)
	}
	tracer := telemetry.NewPgxTracer(cfg)
	acquire := func(wait time.Duration, err error) {
		ctx := tracer.TraceAcquireStart(context.Background(), nil, pgxpool.TraceAcquireStartData{})
		time.Sleep(wait)
		tracer.TraceAcquireEnd(ctx, nil, pgxpool.TraceAcquireEndData{Err: err})
	}
	acquire(0, nil)
	if n := len(recorder.Ended()); n != 0 {
		t.Fatalf("an acquire that did not wait left %d spans, want none", n)
	}
	acquire(5*time.Millisecond, nil)
	acquire(0, context.DeadlineExceeded)

	spans := recorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want one for the wait and one for the failure", len(spans))
	}
	for _, span := range spans {
		if span.Name() != "postgresql.pool.acquire" || attrs(span)["db.client.connection.pool.name"].AsString() != "db.internal:5432/memoh" {
			t.Errorf("span %q with %v, want postgresql.pool.acquire on db.internal:5432/memoh", span.Name(), attrs(span))
		}
	}
	if waited := spans[0].EndTime().Sub(spans[0].StartTime()); waited < 5*time.Millisecond {
		t.Errorf("the wait span lasted %v, want it to cover the 5ms wait", waited)
	}
	if spans[1].Status().Code != codes.Error || attrs(spans[1])["error.type"].AsString() != "context.DeadlineExceeded" {
		t.Errorf("failed acquire span: status %v, error.type %q", spans[1].Status(), attrs(spans[1])["error.type"].AsString())
	}

	waits, ok := metricNames(t, reader)["db.client.connection.wait_time"]
	if !ok {
		t.Fatal("db.client.connection.wait_time not recorded")
	}
	histogram, ok := waits.Data.(metricdata.Histogram[float64])
	if !ok || len(histogram.DataPoints) != 1 {
		t.Fatalf("db.client.connection.wait_time = %+v, want one series", waits.Data)
	}
	point := histogram.DataPoints[0]
	name, _ := point.Attributes.Value("db.client.connection.pool.name")
	if point.Count != 3 || name.AsString() != "db.internal:5432/memoh" || point.Bounds[0] >= 0.001 {
		t.Errorf("wait time point: count %d, pool %q, first bound %v; want 3 acquires on db.internal:5432/memoh with sub-millisecond buckets", point.Count, name.AsString(), point.Bounds[0])
	}
}

// pgxpool finds the acquire hook on the tracer installed on its connection
// config; a pool that cannot connect reports the failed acquire.
func TestPgxTracerIsThePoolsAcquireTracer(t *testing.T) {
	recorder := recordSpans(t)
	cfg, err := pgxpool.ParseConfig("postgres://memoh@127.0.0.1:1/memoh?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = telemetry.NewPgxTracer(cfg.ConnConfig)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Acquire(context.Background()); err == nil {
		t.Fatal("acquired a connection from an unreachable server")
	}
	for _, span := range recorder.Ended() {
		if span.Name() == "postgresql.pool.acquire" && span.Status().Code == codes.Error {
			return
		}
	}
	t.Fatalf("no failed postgresql.pool.acquire span among %d spans", len(recorder.Ended()))
}
