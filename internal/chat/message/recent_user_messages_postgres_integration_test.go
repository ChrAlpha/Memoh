package message

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

func TestPostgresRecentUserMessagesReadVisibleUserRowsBeforeCutoff(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)
	svc := NewService(nil, postgresstore.NewQueries(dbsqlc.New(tx)))
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	insert := func(position int64, role, text, metadata string, visible bool) string {
		t.Helper()
		id := uuid.NewString()
		if _, err := tx.Exec(ctx, `INSERT INTO bot_history_messages
   (id, bot_id, session_id, role, content, metadata, source_message_id, created_at, turn_position, turn_message_seq, turn_id, turn_visible)
   VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7, $8, $9, 0, gen_random_uuid(), $10)`,
			id, postgresMessageTestBotID, postgresMessageTestSessionID, role,
			fmt.Sprintf(`{"role":%q,"content":%q}`, role, text), metadata, "external-"+text,
			base.Add(time.Duration(position)*time.Second), position, visible); err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := insert(1, "user", "first", `{}`, true)
	insert(2, "assistant", "reply", `{}`, true)
	insert(3, "user", "passive", `{"trigger_mode":"passive_sync"}`, true)
	hidden := insert(4, "user", "superseded", `{}`, false)
	insert(5, "tool", "result", `{}`, true)
	second := insert(6, "user", "second", `{}`, true)
	third := insert(7, "user", "third", `{}`, true)

	cases := []struct {
		name   string
		before string
		limit  int32
		want   []string
	}{
		{name: "newest within limit", limit: 2, want: []string{second, third}},
		{name: "all visible user rows", limit: 32, want: []string{first, second, third}},
		{name: "before cutoff", before: third, limit: 32, want: []string{first, second}},
		{name: "unknown cutoff", before: uuid.NewString(), limit: 32},
		{name: "invisible cutoff", before: hidden, limit: 32},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.ListRecentUserBySession(ctx, postgresMessageTestSessionID, tc.before, tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]string, 0, len(got))
			for _, m := range got {
				ids = append(ids, m.ID)
				if m.Role != "user" || m.SessionID != postgresMessageTestSessionID || m.Platform != "local" {
					t.Fatalf("row %s projected role=%q session=%q platform=%q", m.ID, m.Role, m.SessionID, m.Platform)
				}
				if m.Metadata != nil || len(m.RawMetadata) != 0 || m.Assets != nil {
					t.Fatalf("row %s loaded metadata or assets", m.ID)
				}
			}
			if !slices.Equal(ids, tc.want) {
				t.Fatalf("recent user rows = %v, want %v", ids, tc.want)
			}
		})
	}

	got, err := svc.ListRecentUserBySession(ctx, postgresMessageTestSessionID, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ExternalMessageID != "external-third" || string(got[0].Content) != `{"role": "user", "content": "third"}` {
		t.Fatalf("newest user row = %+v", got)
	}

	// Explicit query scoping must also hold when the test connection bypasses RLS.
	if _, err := tx.Exec(ctx, "SELECT set_config('memoh.team_id', $1, true)", uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	got, err = svc.ListRecentUserBySession(ctx, postgresMessageTestSessionID, "", 32)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("cross-team history leaked %d rows", len(got))
	}
}
