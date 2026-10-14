-- name: GetDiscussCoverPosition :one
-- The discuss cursor: the highest history position a run in one of the given
-- states covered. Runs that carry no cover_position never move it.
SELECT COALESCE(MAX((input_json->>'cover_position')::bigint), 0)::bigint AS cover_position
FROM session_runs
WHERE team_id = public.memoh_current_team_id()
  AND session_id = sqlc.arg(session_id)
  AND state = ANY(sqlc.arg(states)::text[])
  AND input_json ? 'cover_position';

-- name: GetDiscussPendingHead :one
-- The newest external input past the cover position. Only rows written with
-- their session event are external input; rows refused at admission have none,
-- and the bot's own output is stored as assistant rows.
SELECT id, turn_position, sender_channel_identity_id
FROM bot_history_messages
WHERE team_id = public.memoh_current_team_id()
  AND session_id = sqlc.arg(session_id)
  AND event_id IS NOT NULL
  AND role = 'user'
  AND turn_position > sqlc.arg(cover_position)
ORDER BY turn_position DESC
LIMIT 1;

-- name: GetDiscussPendingAddressed :one
-- Whether any external input past the cover position mentions the bot or
-- replies to it. The flags live on the input's session event.
SELECT EXISTS (
  SELECT 1
  FROM bot_history_messages m
  JOIN bot_session_events e ON e.id = m.event_id
  WHERE m.team_id = public.memoh_current_team_id()
    AND m.session_id = sqlc.arg(session_id)
    AND m.role = 'user'
    AND m.turn_position > sqlc.arg(cover_position)
    AND (
      COALESCE((e.event_data->>'mentions_me')::boolean, false)
      OR COALESCE((e.event_data->>'replies_to_me')::boolean, false)
    )
)::boolean AS addressed;

-- name: GetDiscussSession :one
-- What a discuss admission needs about the session: its type and runtime, the
-- runtime owner, and the route it answers on.
SELECT
  s.type AS session_type,
  s.runtime_type,
  COALESCE(s.runtime_metadata->>'runtime_owner_account_id', '')::text AS runtime_owner_account_id,
  r.id AS route_id,
  r.channel_type,
  r.conversation_type,
  r.default_reply_target AS reply_target
FROM bot_sessions s
LEFT JOIN bot_channel_routes r ON r.id = s.route_id
WHERE s.team_id = public.memoh_current_team_id()
  AND s.id = sqlc.arg(session_id)
  AND s.bot_id = sqlc.arg(bot_id)
  AND s.deleted_at IS NULL;
