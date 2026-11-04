-- 0175_subagent_fork_context
-- A forked subagent's inherited context becomes part of its history. The rows
-- its fork copied stay out of the chat view (turn_visible = false), but they
-- are context rows of the child: older than its own turns, read by its
-- history loads and folded by its checkpoints like any other row.
--
-- bot_context_history_messages names a session's context rows: the rows of
-- its visible turns and a forked subagent's inherited rows. The two arms are
-- disjoint and each keeps its own partial index
-- (idx_bot_history_messages_visible_session_order and
-- idx_bot_history_messages_subagent_fork_context).
CREATE OR REPLACE VIEW public.bot_context_history_messages
WITH (security_invoker = true) AS
SELECT
  m.team_id,
  m.turn_id,
  m.turn_position,
  m.turn_message_seq,
  m.id,
  m.bot_id,
  m.session_id,
  m.sender_channel_identity_id,
  m.sender_account_user_id,
  m.source_message_id,
  m.source_reply_to_message_id,
  m.role,
  m.content,
  m.metadata,
  m.usage,
  m.session_mode,
  m.runtime_type,
  m.model_id,
  m.event_id,
  m.display_text,
  m.created_at
FROM public.bot_history_messages m
WHERE m.turn_visible = true
  AND m.turn_id IS NOT NULL
  AND m.turn_position IS NOT NULL
  AND m.turn_message_seq IS NOT NULL
UNION ALL
SELECT
  m.team_id,
  m.turn_id,
  m.turn_position,
  m.turn_message_seq,
  m.id,
  m.bot_id,
  m.session_id,
  m.sender_channel_identity_id,
  m.sender_account_user_id,
  m.source_message_id,
  m.source_reply_to_message_id,
  m.role,
  m.content,
  m.metadata,
  m.usage,
  m.session_mode,
  m.runtime_type,
  m.model_id,
  m.event_id,
  m.display_text,
  m.created_at
FROM public.bot_history_messages m
WHERE m.turn_visible = false
  AND m.session_mode = 'subagent'
  AND m.metadata->>'context_scope' = 'subagent_fork'
  AND m.turn_id IS NOT NULL
  AND m.turn_position IS NOT NULL
  AND m.turn_message_seq IS NOT NULL;

-- Until now a forked subagent's inherited rows reached its requests outside
-- its history: prepended ahead of whatever its checkpoint rendered. A child
-- whose own turns were already compacted has checkpoints positioned after
-- those rows, which a load would now pass over. Each such checkpoint keeps the
-- inherited rows by reference ahead of its items, so its requests render
-- exactly what they rendered before, and a later fold can fold them.
DO $subagent_fork_context$
BEGIN
  -- Both tables are under FORCE row level security keyed on memoh.team_id,
  -- which a migration connection never sets. Lift it for the backfill and
  -- restore it after, as 0170 does.
  ALTER TABLE public.bot_history_messages NO FORCE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_messages DISABLE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_checkpoints NO FORCE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_checkpoints DISABLE ROW LEVEL SECURITY;

  WITH inherited AS (
    SELECT m.team_id, m.session_id,
      MAX(m.turn_position) AS last_turn_position,
      pg_catalog.jsonb_agg(
        pg_catalog.jsonb_build_object('row', m.id::text)
        ORDER BY m.turn_position, m.turn_message_seq, m.created_at, m.id
      ) AS items
    FROM public.bot_history_messages m
    WHERE m.turn_visible = false
      AND m.session_mode = 'subagent'
      AND m.metadata->>'context_scope' = 'subagent_fork'
      AND m.turn_id IS NOT NULL
      AND m.turn_position IS NOT NULL
      AND m.turn_message_seq IS NOT NULL
    GROUP BY m.team_id, m.session_id
  )
  UPDATE public.bot_history_checkpoints checkpoint
  SET items = inherited.items || checkpoint.items
  FROM inherited
  WHERE checkpoint.team_id = inherited.team_id
    AND checkpoint.session_id = inherited.session_id
    -- Each inherited row is a turn of its own; only a checkpoint past the
    -- last of them passes over them.
    AND checkpoint.turn_position > inherited.last_turn_position
    AND NOT checkpoint.items @> pg_catalog.jsonb_build_array(inherited.items->0);

  ALTER TABLE public.bot_history_checkpoints ENABLE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_checkpoints FORCE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_messages ENABLE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_messages FORCE ROW LEVEL SECURITY;
END
$subagent_fork_context$;
