package message

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

func TestPostgresTurnResponseSourcesProjectMetadataAndPreserveWindow(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)
	queries := dbsqlc.New(tx)
	svc := NewService(nil, postgresstore.NewQueries(queries))
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Keep content tiny while the audit, usage and display fields are large.
	// Only a JSON boolean true is an interrupted checkpoint, not the string "true".
	flags := []string{`true`, `false`, `"true"`, `null`}
	for i, flag := range flags {
		_, err := tx.Exec(ctx, `INSERT INTO bot_history_messages
   (id, bot_id, session_id, role, content, metadata, usage, display_text, created_at, turn_position, turn_message_seq, turn_id, turn_visible)
   VALUES ($1, $2, $3, 'assistant', $4::jsonb,
    jsonb_build_object('agent_step_interrupted', $5::jsonb,
      'context_lifecycle', jsonb_build_object('selection_decisions', repeat('x', 2*1024*1024))),
    jsonb_build_object('unused', repeat('u', 1024)), repeat('d', 1024), $6, $7, 0, gen_random_uuid(), true)`,
			uuid.NewString(), postgresMessageTestBotID, postgresMessageTestSessionID,
			fmt.Sprintf(`{"role":"assistant","content":"reply-%d"}`, i), flag,
			base.Add(time.Duration(len(flags)-i)*time.Second), int64(i))
		if err != nil {
			t.Fatal(err)
		}
	}
	// Neither passive-sync nor superseded rows may enter the budget/window.
	for _, excluded := range []struct {
		metadata string
		visible  bool
	}{
		{`{"trigger_mode":"passive_sync"}`, true}, {`{}`, false},
	} {
		if _, err := tx.Exec(ctx, `INSERT INTO bot_history_messages
   (id,bot_id,session_id,role,content,metadata,created_at,turn_position,turn_message_seq,turn_visible,turn_id)
   VALUES ($1,$2,$3,'assistant','{"role":"assistant","content":"excluded"}', $4::jsonb,$5,99,0,$6,gen_random_uuid())`,
			uuid.NewString(), postgresMessageTestBotID, postgresMessageTestSessionID, excluded.metadata, base, excluded.visible); err != nil {
			t.Fatal(err)
		}
	}
	for _, since := range []time.Time{time.Unix(0, 0), base.Add(3 * time.Second)} {
		for _, budget := range []int64{0, 1, 100, 1 << 20} {
			t.Run(fmt.Sprintf("since_%d/budget_%d", since.Unix(), budget), func(t *testing.T) {
				got, err := svc.ListTurnResponseSourcesSinceBySessionWithinBytes(ctx, postgresMessageTestSessionID, since, budget)
				if err != nil {
					t.Fatal(err)
				}
				if len(got) == 0 && budget > 0 {
					t.Fatal("a positive budget must load the newest row")
				}
				for i, m := range got {
					if m.CreatedAt.Before(since) {
						t.Fatalf("row %d precedes the window start", i)
					}
					var body struct {
						Content string `json:"content"`
					}
					if err := json.Unmarshal(m.Content, &body); err != nil {
						t.Fatal(err)
					}
					var idx int
					if _, err := fmt.Sscanf(body.Content, "reply-%d", &idx); err != nil {
						t.Fatalf("row %d is not an active reply: %q", i, body.Content)
					}
					if m.Metadata[AgentStepInterruptedMetadataKey] != (flags[idx] == `true`) {
						t.Fatalf("interrupted checkpoint changed at row %d", i)
					}
					if len(m.Metadata) != 1 || len(m.RawMetadata) != 0 || len(m.Usage) != 0 || m.DisplayContent != "" || m.Assets != nil {
						t.Fatalf("row %d retained unused metadata/display/assets", i)
					}
				}
				encoded, err := json.Marshal(got)
				if err != nil {
					t.Fatal(err)
				}
				if len(encoded) > 8192 {
					t.Fatalf("lightweight history serialized to %d bytes", len(encoded))
				}
				if since.Equal(time.Unix(0, 0)) && budget == 1<<20 && len(got) != len(flags) {
					t.Fatalf("loaded %d rows, want %d active rows", len(got), len(flags))
				}
			})
		}
	}
	// Explicit query scoping must also hold when the test connection bypasses RLS.
	if _, err := tx.Exec(ctx, "SELECT set_config('memoh.team_id', $1, true)", uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	got, err := svc.ListTurnResponseSourcesSinceBySessionWithinBytes(ctx, postgresMessageTestSessionID, time.Unix(0, 0), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("cross-team history leaked %d rows", len(got))
	}
}
