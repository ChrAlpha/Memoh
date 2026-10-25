-- 0172_history_checkpoint_omissions
-- Let a history checkpoint leave history out without a summary, and record
-- what it leaves out: how many context rows, and when the first and the last
-- of them was written. A request steps over the rows its bot recorded before
-- it kept history under checkpoints (bots.history_checkpoints_since) when it
-- cannot render them, and a notice in its context reports them
-- (docs/agent-runtime.md). What a checkpoint omits carries over to every
-- checkpoint folded onto it. A checkpoint stands for its history with a
-- summary, an omission, or both.

ALTER TABLE public.bot_history_checkpoints
  ADD COLUMN IF NOT EXISTS omitted_count BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS omitted_first_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS omitted_last_at TIMESTAMPTZ;

ALTER TABLE public.bot_history_checkpoints
  DROP CONSTRAINT IF EXISTS bot_history_checkpoints_omitted_check,
  DROP CONSTRAINT IF EXISTS bot_history_checkpoints_stands_check,
  DROP CONSTRAINT IF EXISTS bot_history_checkpoints_summary_check;

ALTER TABLE public.bot_history_checkpoints
  ADD CONSTRAINT bot_history_checkpoints_omitted_check CHECK (
    (omitted_count = 0 AND omitted_first_at IS NULL AND omitted_last_at IS NULL)
    OR (omitted_count > 0 AND omitted_first_at <= omitted_last_at)
  ),
  ADD CONSTRAINT bot_history_checkpoints_stands_check CHECK (
    pg_catalog.jsonb_path_exists(items, '$[*].summary') OR omitted_count > 0
  );
