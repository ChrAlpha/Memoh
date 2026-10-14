package inbound

import (
	"context"
	"testing"

	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/channel/adapters/telegram"
)

// total is every answer the server gave to a wake.
func (r *recordingAdmitter) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.outcomes) + len(r.errs)
}

// The bot's own message, delivered back by the platform, is not input: it
// wakes nothing, starts no run, and leaves neither an event nor a history row,
// because the reply that produced it is already in history. Whoever speaks
// next is still answered.
func TestDiscussE2EBotsOwnMessageIsNotInput(t *testing.T) {
	e := newDiscussE2E(t, e2eEmbedded, &e2eStep{send: "The freeze is Friday."}, &e2eStep{})
	e.registry = channel.NewRegistry()
	e.registry.MustRegister(policyOnlyAdapter{Adapter: telegram.NewTelegramAdapter(nil)})
	e.cfg.SelfIdentity = map[string]any{"user_id": e.subject("memohbot")}
	e.connect(e2eEmbedded)

	e.mustSay("alice", "m1", "is the freeze on this week?", false)
	hw := e.position()
	eventually(t, "the first message to be answered", func() bool { return e.cover() == hw })
	if got := e.sender.delivered(); len(got) != 1 {
		t.Fatalf("delivered = %q, want the one answer", got)
	}
	events := e.count(`SELECT count(*) FROM bot_session_events WHERE session_id = $1`)
	rows := e.count(`SELECT count(*) FROM bot_history_messages WHERE session_id = $1 AND role = 'user'`)
	wakes, calls := e.admitter.total(), len(e.model.calls())

	e.mustSay("memohbot", "m2", "The freeze is Friday.", true)
	settled()

	if got := e.count(`SELECT count(*) FROM bot_session_events WHERE session_id = $1`); got != events {
		t.Fatalf("events = %d after the bot's own message, want %d", got, events)
	}
	if got := e.count(`SELECT count(*) FROM bot_history_messages WHERE session_id = $1 AND role = 'user'`); got != rows {
		t.Fatalf("user rows = %d after the bot's own message, want %d", got, rows)
	}
	if got := e.count(`SELECT count(*) FROM bot_history_messages WHERE session_id = $1 AND source_message_id = 'm2'`); got != 0 {
		t.Fatalf("rows for the bot's own message = %d, want none", got)
	}
	if got := e.admitter.total(); got != wakes {
		t.Fatalf("wakes = %d after the bot's own message, want %d", got, wakes)
	}
	if got := len(e.model.calls()); got != calls {
		t.Fatalf("model calls = %d after the bot's own message, want %d", got, calls)
	}
	if got := e.runs(); got["completed"] != 1 || len(got) != 1 {
		t.Fatalf("runs = %v, want the first one alone", got)
	}
	var identities int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM channel_identities WHERE channel_type = $1 AND channel_subject_id = $2`,
		e2ePlatform, e.subject("memohbot")).Scan(&identities); err != nil {
		t.Fatal(err)
	}
	if identities != 0 {
		t.Fatalf("channel identities for the bot's own account = %d, want none", identities)
	}

	e.mustSay("carol", "m3", "who owns the release?", false)
	hw = e.position()
	eventually(t, "the next member's message to be answered", func() bool { return e.cover() == hw })
	if got := e.admitter.count(turn.DiscussAdmitted); got != 2 {
		t.Fatalf("wakes that admitted a run = %d, want 2", got)
	}
}
