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
-- and the bot's own output is stored as assistant rows. A row that a run other
-- than a discuss run took as its request message was answered by that run,
-- whatever state the run is in, so it is not pending. A run is a discuss run
-- when it carries a cover position, the same marker the cover query reads.
SELECT m.id, m.turn_position, m.sender_channel_identity_id
FROM bot_history_messages m
WHERE m.team_id = public.memoh_current_team_id()
  AND m.session_id = sqlc.arg(session_id)
  AND m.event_id IS NOT NULL
  AND m.role = 'user'
  AND m.turn_position > sqlc.arg(cover_position)
  AND NOT EXISTS (
    SELECT 1
    FROM session_runs r
    WHERE r.team_id = m.team_id
      AND r.run_id = m.run_id
      AND NOT (r.input_json ? 'cover_position')
  )
ORDER BY m.turn_position DESC
LIMIT 1;

-- name: GetDiscussPendingAddressed :one
-- Whether any external input past the cover position mentions the bot or
-- replies to it. The flags live on the input's session event. Input a non-discuss
-- run already answered is not pending, as in GetDiscussPendingHead.
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
    AND NOT EXISTS (
      SELECT 1
      FROM session_runs r
      WHERE r.team_id = m.team_id
        AND r.run_id = m.run_id
        AND NOT (r.input_json ? 'cover_position')
    )
)::boolean AS addressed;

-- name: GetDiscussSession :one
-- What a discuss admission needs about the session: its runtime, the runtime
-- owner, and the route it answers on. A session is answered this way when what
-- starts its runs is every uncovered external event, not when it is named
-- discuss.
SELECT
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
  AND s.trigger_scope = 'uncovered_events'
  AND s.deleted_at IS NULL;

-- name: ListDiscussSweepCandidates :many
-- Discuss sessions whose newest external input since the given time lies past
-- their cover position. The sweep asks the admission about each; whether the
-- input is answered now is the admission's to say.
SELECT m.bot_id, m.session_id
FROM bot_history_messages m
JOIN bot_sessions s ON s.team_id = m.team_id AND s.id = m.session_id
WHERE m.team_id = public.memoh_current_team_id()
  AND m.event_id IS NOT NULL
  AND m.role = 'user'
  AND m.created_at >= sqlc.arg(since)
  AND s.trigger_scope = 'uncovered_events'
  AND s.deleted_at IS NULL
GROUP BY m.bot_id, m.session_id
HAVING MAX(m.turn_position) > COALESCE((
  SELECT MAX((c.input_json->>'cover_position')::bigint)
  FROM session_runs c
  WHERE c.team_id = public.memoh_current_team_id()
    AND c.session_id = m.session_id
    AND c.state = ANY(sqlc.arg(states)::text[])
    AND c.input_json ? 'cover_position'
), 0);
