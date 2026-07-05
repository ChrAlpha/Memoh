-- name: ListRecentAssistantMessagesBySession :many
SELECT
  id,
  role,
  metadata,
  created_at
FROM bot_history_messages
WHERE session_id = sqlc.arg(session_id)
  AND role = 'assistant'
  AND metadata IS NOT NULL
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(max_count);
