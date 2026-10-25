-- 0172_history_checkpoint_omissions
-- Every checkpoint holds a summary again. One that stands for its history by
-- an omission alone has nothing to stand for it without the omission, so it
-- goes, and with it the checkpoints built on it: their sessions fall back to
-- their whole history as context. The rest lose the record of what they omit.

DO $history_checkpoint_omissions$
BEGIN
  -- The table is under FORCE row level security keyed on memoh.team_id,
  -- which a migration connection never sets. Lift it around the delete.
  ALTER TABLE public.bot_history_checkpoints NO FORCE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_checkpoints DISABLE ROW LEVEL SECURITY;
  DELETE FROM public.bot_history_checkpoints
  WHERE NOT pg_catalog.jsonb_path_exists(items, '$[*].summary');
  ALTER TABLE public.bot_history_checkpoints ENABLE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_checkpoints FORCE ROW LEVEL SECURITY;
END
$history_checkpoint_omissions$;

ALTER TABLE public.bot_history_checkpoints
  DROP CONSTRAINT IF EXISTS bot_history_checkpoints_stands_check,
  DROP CONSTRAINT IF EXISTS bot_history_checkpoints_omitted_check,
  DROP CONSTRAINT IF EXISTS bot_history_checkpoints_summary_check;

ALTER TABLE public.bot_history_checkpoints
  DROP COLUMN IF EXISTS omitted_count,
  DROP COLUMN IF EXISTS omitted_first_at,
  DROP COLUMN IF EXISTS omitted_last_at;

ALTER TABLE public.bot_history_checkpoints
  ADD CONSTRAINT bot_history_checkpoints_summary_check CHECK (pg_catalog.jsonb_path_exists(items, '$[*].summary'));
