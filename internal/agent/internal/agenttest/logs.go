package agenttest

import (
	"context"
	"log/slog"
	"sync"
)

// capturedLogs records every record written through a logger built on it.
type CapturedLogs struct {
	mu      sync.Mutex
	records []slog.Record
}

type capturingHandler struct {
	logs  *CapturedLogs
	attrs []slog.Attr
}

// CaptureLogs returns a logger and the records written through it.
func CaptureLogs() (*slog.Logger, *CapturedLogs) {
	logs := &CapturedLogs{}
	return slog.New(capturingHandler{logs: logs}), logs
}

func (capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h capturingHandler) Handle(_ context.Context, record slog.Record) error {
	record = record.Clone()
	record.AddAttrs(h.attrs...)
	h.logs.mu.Lock()
	defer h.logs.mu.Unlock()
	h.logs.records = append(h.logs.records, record)
	return nil
}

func (h capturingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return capturingHandler{logs: h.logs, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h capturingHandler) WithGroup(string) slog.Handler { return h }

// runResults are the result records of agent runs.
func (l *CapturedLogs) RunResults() []slog.Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []slog.Record
	for _, record := range l.records {
		if record.Message == "agent run" {
			out = append(out, record)
		}
	}
	return out
}

func (l *CapturedLogs) AtLevel(level slog.Level) []slog.Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []slog.Record
	for _, record := range l.records {
		if record.Level == level {
			out = append(out, record)
		}
	}
	return out
}

// RecordAttrs flattens the attributes of record into strings.
func RecordAttrs(record slog.Record) map[string]string {
	out := map[string]string{}
	record.Attrs(func(attr slog.Attr) bool {
		out[attr.Key] = attr.Value.String()
		return true
	})
	return out
}
