package sqlc

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"

	embeddeddb "github.com/memohai/memoh/db"
	"github.com/memohai/memoh/internal/config"
	dbpkg "github.com/memohai/memoh/internal/db"
)

func TestListUncompactedMessagesBySessionProjectsDefaultHeadContext(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "memoh.sqlite")

	migrations, err := fs.Sub(embeddeddb.MigrationsFS, "sqlite/migrations")
	if err != nil {
		t.Fatalf("sqlite migrations fs: %v", err)
	}
	if err := dbpkg.RunMigrateTarget(nil, dbpkg.MigrationTarget{Driver: dbpkg.DriverSQLite, DSN: dsn}, migrations, "up", nil); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	conn, err := dbpkg.OpenSQLite(ctx, config.SQLiteConfig{DSN: dsn})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = conn.Close() }()

	const (
		userID     = "00000000-0000-0000-0000-000000100001"
		botID      = "00000000-0000-0000-0000-000000100002"
		sessionID  = "00000000-0000-0000-0000-000000100003"
		routeID    = "00000000-0000-0000-0000-000000100004"
		identityID = "00000000-0000-0000-0000-000000100005"
		rootTurnID = "00000000-0000-0000-0000-000000100006"
		midTurnID  = "00000000-0000-0000-0000-000000100007"
		headTurnID = "00000000-0000-0000-0000-000000100008"
		sideTurnID = "00000000-0000-0000-0000-000000100009"
	)
	execSQL(t, conn, `INSERT INTO users (id, username, display_name) VALUES (?, ?, ?)`, userID, "owner", "Owner")
	execSQL(t, conn, `INSERT INTO bots (id, owner_user_id, type, name) VALUES (?, ?, ?, ?)`, botID, userID, "personal", "test-bot")
	execSQL(t, conn, `
INSERT INTO bot_channel_routes (id, bot_id, channel_type, external_conversation_id, conversation_type, default_reply_target, metadata)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		routeID,
		botID,
		"telegram",
		"chat-1",
		"group",
		"reply-default",
		`{"conversation_name":"Project Room"}`,
	)
	execSQL(t, conn, `
INSERT INTO bot_sessions (id, bot_id, route_id, channel_type, type, session_mode, runtime_type)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sessionID,
		botID,
		routeID,
		"telegram",
		"chat",
		"chat",
		"model",
	)
	execSQL(t, conn, `
INSERT INTO channel_identities (id, channel_type, channel_subject_id, display_name, avatar_url)
VALUES (?, ?, ?, ?, ?)`,
		identityID,
		"telegram",
		"tg-user",
		"Ada",
		"https://example.test/avatar.png",
	)
	for _, turn := range []struct {
		id     string
		parent any
	}{
		{id: rootTurnID, parent: nil},
		{id: midTurnID, parent: rootTurnID},
		{id: headTurnID, parent: midTurnID},
		{id: sideTurnID, parent: rootTurnID},
	} {
		execSQL(t, conn, `
INSERT INTO bot_history_turns (id, bot_id, owner_session_id, parent_turn_id)
VALUES (?, ?, ?, ?)`,
			turn.id,
			botID,
			sessionID,
			turn.parent,
		)
	}
	execSQL(t, conn, `UPDATE bot_sessions SET default_head_turn_id = ? WHERE id = ?`, headTurnID, sessionID)
	execSQL(t, conn, `INSERT INTO bot_session_turn_heads (session_id, head_turn_id, bot_id) VALUES (?, ?, ?)`, sessionID, headTurnID, botID)
	execSQL(t, conn, `INSERT INTO bot_session_turn_heads (session_id, head_turn_id, bot_id) VALUES (?, ?, ?)`, sessionID, sideTurnID, botID)

	insertHistoryMessage(t, conn, historyMessage{
		id:         "00000000-0000-0000-0000-000000100101",
		turnID:     rootTurnID,
		seq:        1,
		role:       "user",
		content:    `{"role":"user","content":"root"}`,
		metadata:   `{}`,
		createdAt:  "2026-06-30 10:00:00",
		display:    "root",
		externalID: "external-root",
	})
	insertHistoryMessage(t, conn, historyMessage{
		id:        "00000000-0000-0000-0000-000000100102",
		turnID:    midTurnID,
		seq:       1,
		role:      "assistant",
		content:   `{"role":"assistant","content":"middle"}`,
		metadata:  `{}`,
		usage:     `{"inputTokens":3,"outputTokens":5}`,
		createdAt: "2026-06-30 10:01:00",
		display:   "middle",
	})
	insertHistoryMessage(t, conn, historyMessage{
		id:          "00000000-0000-0000-0000-000000100103",
		turnID:      headTurnID,
		seq:         1,
		role:        "assistant",
		content:     `{"role":"assistant","content":"head"}`,
		metadata:    `{"kind":"final"}`,
		usage:       `{"inputTokens":7,"outputTokens":11}`,
		createdAt:   "2026-06-30 10:02:00",
		display:     "head",
		replyTarget: "external-root",
	})
	insertHistoryMessage(t, conn, historyMessage{
		id:        "00000000-0000-0000-0000-000000100104",
		turnID:    headTurnID,
		seq:       2,
		role:      "assistant",
		content:   `{"role":"assistant","content":"passive"}`,
		metadata:  `{"trigger_mode":"passive_sync"}`,
		createdAt: "2026-06-30 10:03:00",
		display:   "passive",
	})
	insertHistoryMessage(t, conn, historyMessage{
		id:        "00000000-0000-0000-0000-000000100105",
		turnID:    midTurnID,
		seq:       2,
		role:      "assistant",
		content:   `{"role":"assistant","content":"compacted"}`,
		metadata:  `{}`,
		compactID: "compact-1",
		createdAt: "2026-06-30 10:04:00",
		display:   "compacted",
	})
	insertHistoryMessage(t, conn, historyMessage{
		id:        "00000000-0000-0000-0000-000000100106",
		turnID:    sideTurnID,
		seq:       1,
		role:      "assistant",
		content:   `{"role":"assistant","content":"side"}`,
		metadata:  `{}`,
		createdAt: "2026-06-30 10:05:00",
		display:   "side",
	})

	rows, err := New(conn).ListUncompactedMessagesBySession(ctx, sessionID)
	if err != nil {
		t.Fatalf("list uncompacted messages: %v", err)
	}
	if got, want := messageIDs(rows), []string{
		"00000000-0000-0000-0000-000000100101",
		"00000000-0000-0000-0000-000000100102",
		"00000000-0000-0000-0000-000000100103",
	}; !sameStrings(got, want) {
		t.Fatalf("message ids = %#v, want %#v", got, want)
	}

	root := rows[0]
	if got := root.BotID; got != botID {
		t.Fatalf("root bot id = %q, want %s", got, botID)
	}
	if got := root.SessionID; !got.Valid || got.String != sessionID {
		t.Fatalf("root session id = %#v, want %s", got, sessionID)
	}
	if got := root.ExternalMessageID; !got.Valid || got.String != "external-root" {
		t.Fatalf("root external message id = %#v, want external-root", got)
	}
	if got := root.SourceReplyToMessageID; got.Valid {
		t.Fatalf("root source reply id = %#v, want null", got)
	}

	head := rows[2]
	if got := head.ViewHeadTurnID; !got.Valid || got.String != headTurnID {
		t.Fatalf("view head = %#v, want %s", got, headTurnID)
	}
	if got := head.TurnID; !got.Valid || got.String != headTurnID {
		t.Fatalf("turn id = %#v, want %s", got, headTurnID)
	}
	if got := head.TurnMessageSeq; !got.Valid || got.Int64 != 1 {
		t.Fatalf("turn seq = %#v, want 1", got)
	}
	if got := head.SenderChannelIdentityID; !got.Valid || got.String != identityID {
		t.Fatalf("sender identity = %#v, want %s", got, identityID)
	}
	if got := head.SenderUserID; !got.Valid || got.String != userID {
		t.Fatalf("sender user = %#v, want %s", got, userID)
	}
	if got := head.ExternalMessageID; got.Valid {
		t.Fatalf("head external message id = %#v, want null", got)
	}
	if got := head.SourceReplyToMessageID; !got.Valid || got.String != "external-root" {
		t.Fatalf("head source reply id = %#v, want external-root", got)
	}
	if got := head.Role; got != "assistant" {
		t.Fatalf("role = %q, want assistant", got)
	}
	if got := head.Content; got != `{"role":"assistant","content":"head"}` {
		t.Fatalf("content = %q, want head payload", got)
	}
	if got := head.Metadata; got != `{"kind":"final"}` {
		t.Fatalf("metadata = %q, want final metadata", got)
	}
	if got := head.Usage; !got.Valid || got.String != `{"inputTokens":7,"outputTokens":11}` {
		t.Fatalf("usage = %#v, want token usage", got)
	}
	if got := head.SessionMode; got != "chat" {
		t.Fatalf("session mode = %q, want chat", got)
	}
	if got := head.RuntimeType; got != "model" {
		t.Fatalf("runtime type = %q, want model", got)
	}
	if got := head.EventID; got.Valid {
		t.Fatalf("event id = %#v, want null", got)
	}
	if got := head.SenderDisplayName; !got.Valid || got.String != "Ada" {
		t.Fatalf("sender display = %#v, want Ada", got)
	}
	if got := head.SenderAvatarUrl; !got.Valid || got.String != "https://example.test/avatar.png" {
		t.Fatalf("sender avatar = %#v, want avatar url", got)
	}
	if got := head.Platform; !got.Valid || got.String != "telegram" {
		t.Fatalf("platform = %#v, want telegram", got)
	}
	if got := head.ConversationType; !got.Valid || got.String != "group" {
		t.Fatalf("conversation type = %#v, want group", got)
	}
	if got, ok := head.ConversationName.(string); !ok || got != "Project Room" {
		t.Fatalf("conversation name = %#v, want Project Room", head.ConversationName)
	}
	if got := head.ReplyTarget; !got.Valid || got.String != "reply-default" {
		t.Fatalf("reply target = %#v, want reply-default", got)
	}
	if got := head.DisplayText; !got.Valid || got.String != "head" {
		t.Fatalf("display text = %#v, want head", got)
	}
	if got := head.CompactID; got != nil {
		t.Fatalf("compact id = %#v, want nil", got)
	}
	if got := head.CreatedAt; got != "2026-06-30 10:02:00" {
		t.Fatalf("created at = %q, want 2026-06-30 10:02:00", got)
	}
}

type historyMessage struct {
	id          string
	turnID      string
	seq         int
	role        string
	content     string
	metadata    string
	usage       string
	compactID   string
	createdAt   string
	display     string
	externalID  string
	replyTarget string
}

func insertHistoryMessage(t *testing.T, db *sql.DB, msg historyMessage) {
	t.Helper()
	execSQL(t, db, `
INSERT INTO bot_history_messages (
  id, bot_id, session_id, turn_id, turn_message_seq,
  sender_channel_identity_id, sender_account_user_id,
  source_message_id, source_reply_to_message_id,
  role, content, metadata, usage, compact_id,
  session_mode, runtime_type, display_text, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.id,
		"00000000-0000-0000-0000-000000100002",
		"00000000-0000-0000-0000-000000100003",
		msg.turnID,
		msg.seq,
		"00000000-0000-0000-0000-000000100005",
		"00000000-0000-0000-0000-000000100001",
		nullString(msg.externalID),
		nullString(msg.replyTarget),
		msg.role,
		msg.content,
		msg.metadata,
		nullString(msg.usage),
		nullString(msg.compactID),
		"chat",
		"model",
		nullString(msg.display),
		msg.createdAt,
	)
}

func execSQL(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("exec sql: %v", err)
	}
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func messageIDs(rows []ListUncompactedMessagesBySessionRow) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
