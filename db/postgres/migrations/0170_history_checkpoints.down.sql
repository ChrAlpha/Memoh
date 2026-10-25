-- 0170_history_checkpoints
-- Drop the history checkpoints and what keeps them in step with history.

DROP TRIGGER IF EXISTS bot_history_messages_reappear ON public.bot_history_messages;
DROP TRIGGER IF EXISTS bot_history_messages_appear ON public.bot_history_messages;
DROP FUNCTION IF EXISTS public.memoh_history_row_appeared();
DROP FUNCTION IF EXISTS public.memoh_history_checkpoint_hides(uuid, uuid, bigint, bigint);
DROP FUNCTION IF EXISTS public.memoh_rewind_history_checkpoint(uuid, bigint, bigint);
DROP FUNCTION IF EXISTS public.memoh_history_checkpoint_kept(uuid, bigint, bigint);

ALTER TABLE public.bot_sessions
  DROP CONSTRAINT IF EXISTS bot_sessions_history_checkpoint_id_fkey,
  DROP COLUMN IF EXISTS history_checkpoint_id;
DROP TABLE IF EXISTS public.bot_history_appeared_rows;
DROP TABLE IF EXISTS public.bot_history_checkpoints;
