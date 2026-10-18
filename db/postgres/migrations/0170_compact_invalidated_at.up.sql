-- 0170_compact_invalidated_at
-- Mark a compaction artifact invalidated without advancing the session's
-- compaction_epoch. A retry or edit replaces a turn; only the artifacts whose
-- anchor range reaches the replaced turn's first hidden row, and that cover
-- one of its rows, are invalidated. Artifacts that end before that row stay
-- valid, so the summaries of the history before the turn keep standing. An
-- invalidated artifact is treated as one of an older epoch everywhere: it is
-- not read into context, it does not cover its rows, and it cannot complete.
--
-- Nullable and additive: NULL means valid, which every existing row is.

ALTER TABLE public.bot_history_message_compacts
  ADD COLUMN IF NOT EXISTS invalidated_at TIMESTAMPTZ;
