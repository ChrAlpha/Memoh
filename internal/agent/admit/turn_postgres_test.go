package admit

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/agenttest"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	"github.com/felinics/memoh/internal/agent/turn"
	chatview "github.com/felinics/memoh/internal/agent/view"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
	"github.com/felinics/memoh/internal/runtimefence"
)

// This integration test pins the #1044 chain end to end at the backend: a
// channel-shaped StartTurnCommand admitted through Turn must surface
// its user message in the runtime snapshot a subscriber receives — the same
// snapshot the web frontend renders the live bubble from. It uses the real
// Manager and the real PostgreSQL ledger, so a break anywhere between wiring,
// activation, and snapshot delivery fails here rather than in production.

func TestAdmitTurnRunRequestUserTurnReachesSubscriber(t *testing.T) {
	ctx := context.Background()
	pool := agenttest.OpenPostgres(t, ctx)
	botID, sessionID := agenttest.CreateSessionFixture(t, ctx, pool)

	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID:       "owner-turn-admission-test",
		StateTTL:      time.Minute,
		OwnerLeaseTTL: time.Second,
		Ledger:        ledger.NewPostgres(dbsqlc.New(pool), pool),
		// The durable ledger refuses admission without the fence that orders
		// its writes; wire the same pair the composition root wires.
		Fence: runtimefence.NewActivator(postgresstore.NewQueriesWithPool(pool, dbsqlc.New(pool))),
	})
	t.Cleanup(func() { _ = manager.Close() })

	svc := New(Deps{Runtime: manager})
	cmd := turn.StartTurnCommand{
		TeamID:                  "team-1",
		Mode:                    turn.ModeChat,
		BotID:                   botID,
		ThreadID:                sessionID,
		Query:                   "hello from telegram",
		ExternalMessageID:       "ext-1",
		CurrentChannel:          "telegram",
		DisplayName:             "Alice",
		SourceChannelIdentityID: "ci-1",
	}
	admission, err := svc.Turn(ctx, cmd, func() {}, nil, nil)
	if err != nil {
		t.Fatalf("Turn() error = %v", err)
	}
	t.Cleanup(func() {
		// Close the run's record so the fixture leaves no active row behind;
		// the fixture rows themselves cascade away with the bot.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Second)
		defer cancel()
		_, _ = manager.FinishRun(ctx, admission.Handle, sessionruntime.RunStatusCompleted)
	})

	sub, err := manager.Subscribe(ctx, botID, sessionID)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	t.Cleanup(sub.Close)

	select {
	case event := <-sub.C:
		if event.Type != sessionruntime.EventRuntimeSnapshot {
			t.Fatalf("first event = %q, want %q", event.Type, sessionruntime.EventRuntimeSnapshot)
		}
		if event.Snapshot == nil || event.Snapshot.CurrentRunView == nil {
			t.Fatal("snapshot has no current run, want the admitted run")
		}
		data, err := json.Marshal(event.Snapshot.CurrentRunView)
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			RequestUserTurn *chatview.UITurn `json:"request_user_turn"`
		}
		if err := json.Unmarshal(data, &wire); err != nil {
			t.Fatal(err)
		}
		request := wire.RequestUserTurn
		if request == nil {
			t.Fatal("snapshot request_user_turn is nil — the #1044 regression shape")
		}
		if request.Text != "hello from telegram" {
			t.Fatalf("request_user_turn text = %q, want the channel message", request.Text)
		}
		if request.TurnID != admission.TurnID {
			t.Fatalf("request_user_turn turn id = %q, want the admitted turn %q", request.TurnID, admission.TurnID)
		}
		if request.Platform != "telegram" || request.SenderDisplayName != "Alice" {
			t.Fatalf("platform/sender = (%q, %q), want telegram/Alice", request.Platform, request.SenderDisplayName)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the subscription baseline snapshot")
	}
}
