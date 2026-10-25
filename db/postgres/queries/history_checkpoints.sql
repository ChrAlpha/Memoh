-- name: GetActiveHistoryCheckpoint :one
-- The session's active checkpoint; no row when its whole history is context.
SELECT checkpoint.*
FROM bot_history_checkpoints checkpoint
JOIN bot_sessions session
  ON session.team_id = checkpoint.team_id
 AND session.history_checkpoint_id = checkpoint.id
WHERE session.team_id = public.memoh_current_team_id()
  AND session.id = sqlc.arg(session_id);

-- name: GetHistoryCheckpoint :one
SELECT *
FROM bot_history_checkpoints
WHERE team_id = public.memoh_current_team_id()
  AND id = sqlc.arg(id);

-- name: ListHistoryCheckpointChain :many
-- A checkpoint and its ancestors, newest first.
WITH RECURSIVE chain AS (
  SELECT checkpoint.id, checkpoint.parent_id, 0 AS depth
  FROM bot_history_checkpoints checkpoint
  WHERE checkpoint.team_id = public.memoh_current_team_id()
    AND checkpoint.id = sqlc.arg(id)
  UNION ALL
  SELECT parent.id, parent.parent_id, chain.depth + 1
  FROM chain
  JOIN bot_history_checkpoints parent
    ON parent.team_id = public.memoh_current_team_id()
   AND parent.id = chain.parent_id
)
SELECT checkpoint.*
FROM chain
JOIN bot_history_checkpoints checkpoint
  ON checkpoint.team_id = public.memoh_current_team_id()
 AND checkpoint.id = chain.id
ORDER BY chain.depth;

-- name: CheckHistoryCheckpoint :one
-- What no longer holds of a checkpoint: rows it keeps by reference that are
-- no longer context rows of its session, summaries that are not usable, and
-- context rows that appeared at or before its position after it was made
-- (memoh_history_row_appeared). recorded_rows counts every appeared row its
-- session has on record, which a read forgets once none is hidden.
WITH target AS MATERIALIZED (
  SELECT checkpoint.id, checkpoint.session_id, checkpoint.items
  FROM bot_history_checkpoints checkpoint
  WHERE checkpoint.team_id = public.memoh_current_team_id()
    AND checkpoint.id = sqlc.arg(id)
), item AS MATERIALIZED (
  SELECT jsonb_array_elements(target.items) AS value
  FROM target
)
SELECT
  (
    SELECT count(*)
    FROM item
    CROSS JOIN target
    WHERE item.value ? 'row'
      AND NOT EXISTS (
        SELECT 1
        FROM bot_visible_history_messages m
        WHERE m.team_id = public.memoh_current_team_id()
          AND m.session_id = target.session_id
          AND m.id = (item.value->>'row')::uuid
          AND (m.metadata->>'trigger_mode' IS NULL OR m.metadata->>'trigger_mode' != 'passive_sync')
      )
  )::bigint AS missing_rows,
  (
    SELECT count(*)
    FROM item
    CROSS JOIN target
    WHERE item.value ? 'summary'
      AND NOT EXISTS (
        SELECT 1
        FROM bot_history_message_compacts compact
        WHERE compact.team_id = public.memoh_current_team_id()
          AND compact.session_id = target.session_id
          AND compact.id = (item.value->>'summary')::uuid
          AND compact.status = 'ok'
          AND NULLIF(BTRIM(compact.summary, E' \t\n\r\f\x0B'), '') IS NOT NULL
      )
  )::bigint AS missing_summaries,
  (
    SELECT count(*)
    FROM bot_history_appeared_rows appeared
    CROSS JOIN target
    WHERE appeared.team_id = public.memoh_current_team_id()
      AND appeared.session_id = target.session_id
      AND public.memoh_history_checkpoint_hides(target.id, appeared.checkpoint_id, appeared.turn_position, appeared.turn_message_seq)
  )::bigint AS appeared_rows,
  (
    SELECT count(*)
    FROM bot_history_appeared_rows appeared
    CROSS JOIN target
    WHERE appeared.team_id = public.memoh_current_team_id()
      AND appeared.session_id = target.session_id
  )::bigint AS recorded_rows
FROM target;

-- name: ForgetHistoryAppearedRows :execrows
-- Forgets the rows on record as appearing below the session's checkpoints
-- that checkpoint_id does not hide, all of them when it is NULL, while it is
-- still the session's active checkpoint. Once that one holds, no row on
-- record is hidden, and every checkpoint made after it read them. A row on
-- record against a checkpoint made since the session moved on stays.
DELETE FROM bot_history_appeared_rows appeared
USING bot_sessions session
WHERE appeared.team_id = public.memoh_current_team_id()
  AND appeared.session_id = sqlc.arg(session_id)
  AND session.team_id = public.memoh_current_team_id()
  AND session.id = appeared.session_id
  AND session.history_checkpoint_id IS NOT DISTINCT FROM sqlc.narg(checkpoint_id)::uuid
  AND NOT COALESCE(public.memoh_history_checkpoint_hides(
    sqlc.narg(checkpoint_id)::uuid, appeared.checkpoint_id, appeared.turn_position, appeared.turn_message_seq
  ), false);

-- name: ListHistoryCheckpointSummaries :many
SELECT compact.id, compact.summary
FROM bot_history_message_compacts compact
WHERE compact.team_id = public.memoh_current_team_id()
  AND compact.session_id = sqlc.arg(session_id)
  AND compact.id = ANY(sqlc.arg(ids)::uuid[])
  AND compact.status = 'ok';

-- name: LockHistoryCheckpointSession :one
-- A fold commits under the session's row lock: it waits for every row write
-- in flight, which holds the session's key share through its foreign key.
SELECT session.history_checkpoint_id
FROM bot_sessions session
WHERE session.team_id = public.memoh_current_team_id()
  AND session.id = sqlc.arg(session_id)
  AND session.bot_id = sqlc.arg(bot_id)
FOR UPDATE;

-- name: ListHistoryCheckpointSpanRows :many
-- The context rows a fold's checkpoint accounts for: the rows after its
-- parent's position through its own, and the rows named by ids (the rows its
-- parent keeps by reference and the rows it folds), in turn order.
SELECT m.id, m.turn_position, m.turn_message_seq
FROM bot_visible_history_messages m
WHERE m.team_id = public.memoh_current_team_id()
  AND m.session_id = sqlc.arg(session_id)
  AND (
    (
      (m.turn_position, m.turn_message_seq) > (sqlc.arg(after_turn_position)::bigint, sqlc.arg(after_turn_message_seq)::bigint)
      AND (m.turn_position, m.turn_message_seq) <= (sqlc.arg(through_turn_position)::bigint, sqlc.arg(through_turn_message_seq)::bigint)
    )
    OR m.id = ANY(sqlc.arg(ids)::uuid[])
  )
  AND (m.metadata->>'trigger_mode' IS NULL OR m.metadata->>'trigger_mode' != 'passive_sync')
ORDER BY m.turn_position ASC, m.turn_message_seq ASC, m.created_at ASC, m.id ASC;

-- name: CreateHistoryCheckpoint :one
INSERT INTO bot_history_checkpoints (
  bot_id, session_id, parent_id, turn_position, turn_message_seq, items
)
SELECT session.bot_id, session.id, sqlc.narg(parent_id)::uuid,
  sqlc.arg(turn_position)::bigint, sqlc.arg(turn_message_seq)::bigint,
  sqlc.arg(items)::jsonb
FROM bot_sessions session
WHERE session.team_id = public.memoh_current_team_id()
  AND session.id = sqlc.arg(session_id)
RETURNING *;

-- name: SetHistoryCheckpoint :execrows
-- Moves the session's active checkpoint, only from the one the caller read.
UPDATE bot_sessions
SET history_checkpoint_id = sqlc.narg(checkpoint_id)::uuid
WHERE team_id = public.memoh_current_team_id()
  AND id = sqlc.arg(session_id)
  AND history_checkpoint_id IS NOT DISTINCT FROM sqlc.narg(expected_checkpoint_id)::uuid;

-- name: GetHistoryCheckpointKeptBeforeTurn :one
-- The checkpoint the session keeps once the turn's rows are replaced, which
-- an edit or retry renders before its replacement commits: the active one
-- when the turn has no context rows.
WITH replaced AS MATERIALIZED (
  SELECT MIN(m.turn_position)::bigint AS turn_position,
    MIN(m.turn_message_seq)::bigint AS turn_message_seq
  FROM bot_visible_history_messages m
  WHERE m.team_id = public.memoh_current_team_id()
    AND m.session_id = sqlc.arg(session_id)
    AND m.turn_id = sqlc.arg(turn_id)
    AND (m.metadata->>'trigger_mode' IS NULL OR m.metadata->>'trigger_mode' != 'passive_sync')
)
SELECT (CASE
    WHEN replaced.turn_position IS NULL THEN session.history_checkpoint_id
    ELSE public.memoh_history_checkpoint_kept(session.id, replaced.turn_position, replaced.turn_message_seq)
  END)::uuid AS checkpoint_id
FROM bot_sessions session
CROSS JOIN replaced
WHERE session.team_id = public.memoh_current_team_id()
  AND session.id = sqlc.arg(session_id);

-- name: RewindHistoryCheckpointBeforeTurn :one
-- An edit or retry that compacts before its first step commits rewinds the
-- session as SelectBoundHistoryTurnReplacement will when that step replaces
-- the turn, so its fold builds on the checkpoint the replacement keeps. It
-- reports whether it rewound.
WITH owner_session AS MATERIALIZED (
  SELECT session.id
  FROM bot_sessions session
  WHERE session.team_id = public.memoh_current_team_id()
    AND session.id = sqlc.arg(session_id)
    AND session.bot_id = sqlc.arg(bot_id)
  FOR UPDATE
), replaced AS MATERIALIZED (
  SELECT MIN(m.turn_position)::bigint AS turn_position,
    MIN(m.turn_message_seq)::bigint AS turn_message_seq
  FROM bot_visible_history_messages m
  JOIN owner_session owner ON owner.id = m.session_id
  WHERE m.team_id = public.memoh_current_team_id()
    AND m.turn_id = sqlc.arg(turn_id)
    AND (m.metadata->>'trigger_mode' IS NULL OR m.metadata->>'trigger_mode' != 'passive_sync')
)
SELECT COALESCE((
  SELECT public.memoh_rewind_history_checkpoint(owner.id, replaced.turn_position, replaced.turn_message_seq)
  FROM replaced
  WHERE replaced.turn_position IS NOT NULL
), false)::boolean AS rewound
FROM owner_session owner;
