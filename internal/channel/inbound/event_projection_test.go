package inbound

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/chat/timeline"
)

type stampingEventStore struct {
	cursor int64
	err    error
	id     string
}

func (s stampingEventStore) PersistEvent(_ context.Context, _, _ string, event timeline.CanonicalEvent) (string, timeline.CanonicalEvent, error) {
	msg, ok := event.(timeline.MessageEvent)
	if !ok {
		return "", event, errors.New("unexpected event type")
	}
	msg.EventCursor = s.cursor
	return s.id, msg, s.err
}

func inboundTestEvent() timeline.MessageEvent {
	return timeline.MessageEvent{
		SessionID:    "s1",
		MessageID:    "m1",
		ReceivedAtMs: 1000,
		TimestampSec: 1,
		Content:      []timeline.ContentNode{{Type: "text", Text: "hello"}},
		Conversation: timeline.ConversationMeta{Channel: "telegram", ConversationType: "group"},
	}
}

func TestPersistInboundEventReportsCommit(t *testing.T) {
	store := stampingEventStore{cursor: 4242, id: "event-1"}

	id, ok := persistInboundEvent(context.Background(), store, nil, "bot-1", "s1", inboundTestEvent())

	if !ok || id != "event-1" {
		t.Fatalf("persist = (%q, %v), want (event-1, true)", id, ok)
	}
}

func TestPersistInboundEventFailureWarnsAndDoesNotReportCommit(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	store := stampingEventStore{err: errors.New("insert unavailable"), id: "ignored"}

	id, ok := persistInboundEvent(context.Background(), store, log, "bot-1", "s1", inboundTestEvent())

	if ok || id != "" {
		t.Fatalf("failed persist = (%q, %v), want (\"\", false)", id, ok)
	}
	if got := logs.String(); !strings.Contains(got, "WARN") || !strings.Contains(got, "insert unavailable") {
		t.Fatalf("expected a warn log with the cause, got %q", got)
	}
}

func TestPersistInboundEventWithoutStore(t *testing.T) {
	id, ok := persistInboundEvent(context.Background(), nil, nil, "bot-1", "s1", inboundTestEvent())

	if ok || id != "" {
		t.Fatalf("storeless persist = (%q, %v), want (\"\", false)", id, ok)
	}
}
