-- name: CreateCompactionLog :one
-- A compaction event records each pass here before its summarizer call. The
-- log is pending until the pass commits its summary or fails.
INSERT INTO bot_history_message_compacts (bot_id, session_id, team_id)
SELECT session.bot_id, session.id, session.team_id
FROM bot_sessions session
WHERE session.team_id = public.memoh_current_team_id()
  AND session.id = sqlc.arg(session_id)
  AND session.bot_id = sqlc.arg(bot_id)
RETURNING *;

-- name: CompleteCompactionLog :one
-- Settles a pending pass. A pass that commits does it in the transaction that
-- makes its checkpoint the session's active one; no row comes back for a log
-- that is no longer pending.
UPDATE bot_history_message_compacts
SET status = sqlc.arg(status),
    summary = sqlc.arg(summary),
    message_count = sqlc.arg(message_count),
    error_message = sqlc.arg(error_message),
    usage = sqlc.arg(usage),
    model_id = sqlc.arg(model_id),
    completed_at = now()
WHERE team_id = public.memoh_current_team_id()
  AND id = sqlc.arg(id)
  AND status = 'pending'
RETURNING *;

-- name: CopyCompactionSummary :one
-- A fork carries a summary of its source as its own: the same text, model,
-- count and timing, and no usage, so the summarizer call that wrote it is
-- counted once.
INSERT INTO bot_history_message_compacts (
  bot_id, session_id, team_id, status, summary, message_count, model_id, started_at, completed_at
)
SELECT fork.bot_id, fork.id, fork.team_id, 'ok', source.summary, source.message_count,
  source.model_id, source.started_at, source.completed_at
FROM bot_history_message_compacts source
JOIN bot_sessions fork
  ON fork.team_id = public.memoh_current_team_id()
 AND fork.id = sqlc.arg(session_id)
 AND fork.bot_id = source.bot_id
WHERE source.team_id = public.memoh_current_team_id()
  AND source.id = sqlc.arg(source_compact_id)
  AND source.status = 'ok'
RETURNING id;

-- name: ListCompactionLogsByBot :many
SELECT *
FROM bot_history_message_compacts
WHERE team_id = public.memoh_current_team_id() AND bot_id = $1
ORDER BY started_at DESC
LIMIT $2 OFFSET $3;

-- name: CountCompactionLogsByBot :one
SELECT count(*) FROM bot_history_message_compacts WHERE team_id = public.memoh_current_team_id() AND bot_id = $1;

-- name: DeleteCompactionLogsByBot :exec
-- Clearing a bot's compaction logs forgets every summary its sessions hold:
-- their checkpoints go with them, so each session's whole history is context
-- again. A pass still running finds its log or its parent checkpoint gone
-- and fails to commit.
WITH target_sessions AS MATERIALIZED (
  SELECT session.id
  FROM bot_sessions session
  WHERE session.team_id = public.memoh_current_team_id()
    AND session.bot_id = sqlc.arg(target_bot_id)
  ORDER BY session.id
  FOR UPDATE
),
invalidated_sessions AS (
  UPDATE bot_sessions session
  SET history_checkpoint_id = NULL
  FROM target_sessions target
  WHERE session.team_id = public.memoh_current_team_id()
    AND session.id = target.id
  RETURNING session.id
),
deleted_checkpoints AS (
  DELETE FROM bot_history_checkpoints checkpoint
  USING invalidated_sessions invalidated
  WHERE checkpoint.team_id = public.memoh_current_team_id()
    AND checkpoint.session_id = invalidated.id
  RETURNING checkpoint.id
),
target_compaction_logs AS MATERIALIZED (
  SELECT compact.id
  FROM bot_history_message_compacts compact
  WHERE compact.team_id = public.memoh_current_team_id()
    AND compact.bot_id = sqlc.arg(target_bot_id)
    AND (SELECT count(*) FROM invalidated_sessions) >= 0
    AND (SELECT count(*) FROM deleted_checkpoints) >= 0
  ORDER BY compact.id
  FOR UPDATE
)
DELETE FROM bot_history_message_compacts AS compacts
USING target_compaction_logs target
WHERE compacts.team_id = public.memoh_current_team_id()
  AND compacts.id = target.id;

-- name: ReleasePendingCompactionLogsBySession :exec
-- A new runtime owner settles the passes its predecessors left pending: their
-- writes are fenced out, so those passes can never commit.
UPDATE bot_history_message_compacts
SET status = 'error',
    error_message = 'claimant lost runtime ownership',
    completed_at = now()
WHERE team_id = public.memoh_current_team_id()
  AND bot_id = sqlc.arg(bot_id)
  AND session_id = sqlc.arg(session_id)
  AND status = 'pending';
