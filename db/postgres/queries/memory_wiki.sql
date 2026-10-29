-- name: UpsertMemoryNode :one
INSERT INTO memory_nodes (
  id, bot_id, body, hash, layer, fact_type, subject, confidence,
  metadata, source_message_ids, profile_ref, topic, captured_at, expires_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (team_id, id) DO UPDATE SET
  body = EXCLUDED.body,
  hash = EXCLUDED.hash,
  layer = EXCLUDED.layer,
  fact_type = EXCLUDED.fact_type,
  subject = EXCLUDED.subject,
  confidence = EXCLUDED.confidence,
  metadata = EXCLUDED.metadata,
  source_message_ids = (
    SELECT COALESCE(jsonb_agg(ref.value ORDER BY ref.first_seen), '[]'::jsonb)
    FROM (
      SELECT deduplicated.value, deduplicated.first_seen
      FROM (
        SELECT value, min(ordinality) AS first_seen
        FROM jsonb_array_elements(
          COALESCE(memory_nodes.source_message_ids, '[]'::jsonb)
          || COALESCE(EXCLUDED.source_message_ids, '[]'::jsonb)
        ) WITH ORDINALITY AS combined(value, ordinality)
        WHERE jsonb_typeof(value) = 'string'
          AND (value #>> '{}') ~ '^[^/]+/[^/]+$'
        GROUP BY value
      ) AS deduplicated
      ORDER BY deduplicated.first_seen DESC
      -- Keep in sync with adapters.MaxSourceRefsPerMemory.
      LIMIT 64
    ) AS ref
  ),
  profile_ref = EXCLUDED.profile_ref,
  topic = EXCLUDED.topic,
  expires_at = EXCLUDED.expires_at,
  updated_at = now()
WHERE memory_nodes.bot_id = EXCLUDED.bot_id
RETURNING *;

-- name: GetMemoryNode :one
SELECT * FROM memory_nodes
WHERE team_id = public.memoh_current_team_id() AND bot_id = $1 AND id = $2;

-- name: ListMemoryNodesByBot :many
SELECT * FROM memory_nodes
WHERE team_id = public.memoh_current_team_id() AND bot_id = $1
ORDER BY captured_at ASC;

-- name: ListMemoryNodesByBotLayer :many
SELECT * FROM memory_nodes
WHERE team_id = public.memoh_current_team_id() AND bot_id = $1 AND layer = $2
ORDER BY captured_at ASC;

-- name: ListMemoryNodesByBotProfile :many
SELECT * FROM memory_nodes
WHERE team_id = public.memoh_current_team_id() AND bot_id = $1 AND profile_ref = $2
ORDER BY captured_at ASC;

-- name: DeleteMemoryNode :exec
DELETE FROM memory_nodes
WHERE team_id = public.memoh_current_team_id() AND bot_id = $1 AND id = $2;

-- name: DeleteAllMemoryNodesByBot :exec
DELETE FROM memory_nodes
WHERE team_id = public.memoh_current_team_id() AND bot_id = $1;

-- name: CountMemoryNodesByBot :one
SELECT COUNT(*) FROM memory_nodes
WHERE team_id = public.memoh_current_team_id() AND bot_id = $1;
