-- 0175_subagent_fork_context
-- Restore a forked subagent's inherited rows to context outside its history:
-- checkpoints no longer keep them by reference, and the context row view goes.
DO $subagent_fork_context$
BEGIN
  ALTER TABLE public.bot_history_messages NO FORCE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_messages DISABLE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_checkpoints NO FORCE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_checkpoints DISABLE ROW LEVEL SECURITY;

  UPDATE public.bot_history_checkpoints checkpoint
  SET items = COALESCE((
    SELECT pg_catalog.jsonb_agg(entry.item ORDER BY entry.ordinality)
    FROM pg_catalog.jsonb_array_elements(checkpoint.items) WITH ORDINALITY AS entry(item, ordinality)
    WHERE NOT EXISTS (
      SELECT 1
      FROM public.bot_history_messages m
      WHERE m.team_id = checkpoint.team_id
        AND m.session_id = checkpoint.session_id
        AND m.id::text = entry.item->>'row'
        AND m.turn_visible = false
        AND m.session_mode = 'subagent'
        AND m.metadata->>'context_scope' = 'subagent_fork'
    )
  ), '[]'::jsonb)
  WHERE EXISTS (
    SELECT 1
    FROM public.bot_history_messages m
    WHERE m.team_id = checkpoint.team_id
      AND m.session_id = checkpoint.session_id
      AND m.turn_visible = false
      AND m.session_mode = 'subagent'
      AND m.metadata->>'context_scope' = 'subagent_fork'
  );

  ALTER TABLE public.bot_history_checkpoints ENABLE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_checkpoints FORCE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_messages ENABLE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_messages FORCE ROW LEVEL SECURITY;
END
$subagent_fork_context$;

DROP VIEW IF EXISTS public.bot_context_history_messages;
