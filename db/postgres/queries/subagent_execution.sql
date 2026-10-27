-- name: ListExternalSubagentExecutionHolds :many
-- Holds survive run failure, server restart and soft deletion of the thread.
SELECT * FROM bot_sessions
WHERE team_id = public.memoh_current_team_id()
  AND bot_id = sqlc.arg(bot_id)
  AND session_mode = 'subagent'
  AND COALESCE(runtime_metadata->>'external_execution_run', '') <> '';

-- name: BeginExternalSubagentExecution :one
-- Called in a transaction after locking the owning bot and counting holds.
UPDATE bot_sessions
SET runtime_metadata = runtime_metadata || jsonb_build_object('external_execution_run', sqlc.arg(run_id)::text, 'external_child_run', sqlc.arg(run_id)::text)
WHERE team_id = public.memoh_current_team_id()
  AND id = sqlc.arg(session_id)
  AND bot_id = sqlc.arg(bot_id)
  AND session_mode = 'subagent'
  AND runtime_type = sqlc.arg(runtime_type)
  AND runtime_fencing_token = sqlc.arg(fencing_token)
  AND deleted_at IS NULL
  AND COALESCE(runtime_metadata->>'external_execution_run', '') = ''
  AND runtime_metadata->>'external_child_run' IS NULL
RETURNING *;

-- name: FinishExternalSubagentExecution :execrows
-- Release only the exact execution whose driver confirmed its process exit.
UPDATE bot_sessions
SET runtime_metadata = runtime_metadata - 'external_execution_run' - 'external_termination_unconfirmed' - 'external_termination_reap'
WHERE team_id = public.memoh_current_team_id()
  AND id = sqlc.arg(session_id)
  AND runtime_metadata->>'external_execution_run' = sqlc.arg(run_id)::text
  AND runtime_fencing_token = sqlc.arg(fencing_token);

-- name: ClaimExternalSubagentReap :one
UPDATE bot_sessions AS child
SET runtime_metadata = runtime_metadata || jsonb_build_object('external_termination_reap', sqlc.arg(claim)::text),
    updated_at = clock_timestamp()
WHERE child.team_id = public.memoh_current_team_id()
  AND child.id = sqlc.arg(session_id)
  AND child.runtime_metadata->>'external_execution_run' = sqlc.arg(run_id)::text
  AND (child.runtime_metadata->>'external_termination_reap' IS NULL
    OR child.updated_at < clock_timestamp() - interval '60 seconds')
  AND NOT EXISTS (
    SELECT 1 FROM session_runs run
    WHERE run.team_id = child.team_id AND run.session_id = child.id
      AND run.state IN ('accepted', 'running', 'waiting_decision', 'finishing')
  )
RETURNING *;

-- name: ResolveExternalSubagentTermination :one
UPDATE bot_sessions
SET runtime_metadata = (runtime_metadata - 'external_execution_run' - 'external_termination_unconfirmed' - 'external_termination_reap')
    || jsonb_build_object('external_termination_resolved', sqlc.arg(run_id)::text),
    updated_at = clock_timestamp()
WHERE team_id = public.memoh_current_team_id()
  AND id = sqlc.arg(session_id)
  AND runtime_metadata->>'external_execution_run' = sqlc.arg(run_id)::text
  AND runtime_metadata->>'external_termination_reap' = sqlc.arg(claim)::text
RETURNING *;

-- name: ListSubagentRunUsage :many
-- Usage is result evidence, never replayable runtime context. Run identity
-- keeps a later failed follow-up from borrowing an earlier turn's usage.
SELECT usage FROM bot_history_messages
WHERE team_id = public.memoh_current_team_id()
  AND session_id = sqlc.arg(session_id)
  AND run_id = sqlc.arg(run_id)
  AND role = 'assistant'
  AND usage IS NOT NULL
ORDER BY turn_position DESC, turn_message_seq DESC, created_at DESC, id DESC;
