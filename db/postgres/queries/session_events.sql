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
  received_at_ms
) VALUES ($1, $2, $3, $4, $5, $6, $7)
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

-- name: ListSessionReplayFoldedMessageIDs :many
-- The inbound messages a session's checkpoint stands for whole: every context
-- row lies at or before the checkpoint's position without being kept by
-- reference, and the message did not change after its rows were written. A
-- folded message that changed replays whole: its summary read the rows, never
-- the change. The change is the events after the one its rows were written
-- from or, when that cursor is unknown, an edit. Replay runs this once and
-- leaves these messages' events out of every page.
WITH folded AS MATERIALIZED (
  SELECT
    m.source_message_id AS external_message_id,
    COALESCE(MAX((row_event.event_data->>'event_cursor')::bigint), 0)::bigint AS event_cursor
  FROM bot_visible_history_messages m
  LEFT JOIN bot_session_events row_event
    ON row_event.team_id = public.memoh_current_team_id()
   AND row_event.id = m.event_id
  WHERE m.team_id = public.memoh_current_team_id()
    AND m.session_id = sqlc.arg(session_id)
    AND m.source_message_id IS NOT NULL
    AND (m.metadata->>'trigger_mode' IS NULL OR m.metadata->>'trigger_mode' != 'passive_sync')
  GROUP BY m.source_message_id
  HAVING NOT bool_or(
    (m.turn_position, m.turn_message_seq) > (sqlc.arg(after_turn_position)::bigint, sqlc.arg(after_turn_message_seq)::bigint)
    OR m.id = ANY(sqlc.arg(kept_ids)::uuid[])
  )
)
SELECT folded.external_message_id::text AS external_message_id
FROM folded
JOIN bot_session_events event
  ON event.team_id = public.memoh_current_team_id()
 AND event.session_id = sqlc.arg(session_id)
 AND event.external_message_id = folded.external_message_id
GROUP BY folded.external_message_id, folded.event_cursor
HAVING NOT CASE
    WHEN folded.event_cursor > 0
      THEN MAX(COALESCE((event.event_data->>'event_cursor')::bigint, 0)) > folded.event_cursor
    ELSE bool_or(event.event_kind = 'edit')
  END;

-- name: ListSessionEventsBySessionPageBeforeWithinBytes :many
-- Replay reads newest-first with a stable keyset and a hard page-byte budget.
-- The events of the messages the checkpoint stands for whole are excluded in
-- SQL before event_data reaches the process. The page is cut by the keyset
-- before any payload is measured: an event's cumulative bytes count only the
-- events ahead of it, so measuring the page alone admits exactly what
-- measuring every older event would.
WITH page AS MATERIALIZED (
  SELECT event.id
  FROM bot_session_events event
  WHERE event.team_id = public.memoh_current_team_id()
    AND event.session_id = sqlc.arg(session_id)
    AND (
      NOT sqlc.arg(has_cursor)::boolean
      OR (event.received_at_ms, event.created_at, event.id) < (
        sqlc.arg(before_received_at_ms)::BIGINT,
        sqlc.arg(before_created_at)::TIMESTAMPTZ,
        sqlc.arg(before_id)::UUID
      )
    )
    AND NOT EXISTS (
      SELECT 1
      FROM unnest(sqlc.arg(folded_message_ids)::text[]) AS folded(external_message_id)
      WHERE folded.external_message_id = event.external_message_id
    )
  ORDER BY event.received_at_ms DESC, event.created_at DESC, event.id DESC
  LIMIT sqlc.arg(page_size)
), measured AS (
  SELECT
    event.*,
    octet_length(event.event_data::text)::BIGINT AS payload_bytes
  FROM page
  JOIN bot_session_events event ON event.id = page.id
), ranked AS (
  SELECT
    measured.*,
    SUM(payload_bytes) OVER (
      ORDER BY received_at_ms DESC, created_at DESC, id DESC
    )::BIGINT AS cumulative_bytes
  FROM measured
)
SELECT
  id,
  team_id,
  bot_id,
  session_id,
  event_kind,
  event_data,
  external_message_id,
  sender_channel_identity_id,
  received_at_ms,
  created_at,
  payload_bytes,
  cumulative_bytes
FROM ranked
WHERE cumulative_bytes <= sqlc.arg(max_bytes)::BIGINT
ORDER BY received_at_ms DESC, created_at DESC, id DESC;

-- name: ListSessionEventsByBot :many
SELECT * FROM bot_session_events
WHERE team_id = public.memoh_current_team_id() AND bot_id = $1
ORDER BY received_at_ms ASC, id ASC;

-- name: CountSessionEvents :one
SELECT COUNT(*) FROM bot_session_events
WHERE team_id = public.memoh_current_team_id() AND session_id = $1;

-- name: DeleteSessionEventsByBot :exec
DELETE FROM bot_session_events
WHERE team_id = public.memoh_current_team_id() AND bot_id = $1;
