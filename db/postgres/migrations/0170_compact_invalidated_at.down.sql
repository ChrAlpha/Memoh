-- 0170_compact_invalidated_at (down)
ALTER TABLE public.bot_history_message_compacts
  DROP COLUMN IF EXISTS invalidated_at;
