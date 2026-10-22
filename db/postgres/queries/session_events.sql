-- name: NextSessionEventCursor :one
SELECT nextval('bot_session_event_cursor_seq')::bigint;

-- name: CreateSessionEvent :one
INSERT INTO bot_session_events (
  bot_id,
  session_id,
  event_kind,
  event_data,
  external_message_id,
  sender_channel_identity_id,
  received_at_ms,
  mentions_me,
  replies_to_me
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT DO NOTHING
RETURNING id;

-- name: ListSessionEventsBySession :many
SELECT * FROM bot_session_events
WHERE team_id = public.memoh_current_team_id() AND session_id = $1
ORDER BY received_at_ms ASC, created_at ASC, id ASC;

-- name: ListSessionEventsBySessionAfter :many
SELECT * FROM bot_session_events
WHERE team_id = public.memoh_current_team_id() AND session_id = $1 AND received_at_ms >= $2
ORDER BY received_at_ms ASC, created_at ASC, id ASC;

-- name: ListSessionEventsByBot :many
SELECT * FROM bot_session_events
WHERE team_id = public.memoh_current_team_id() AND bot_id = $1
ORDER BY received_at_ms ASC, id ASC;

-- name: DeleteSessionEventsByBot :exec
DELETE FROM bot_session_events
WHERE team_id = public.memoh_current_team_id() AND bot_id = $1;
