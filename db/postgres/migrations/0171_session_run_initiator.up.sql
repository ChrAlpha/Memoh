-- 0171_session_run_initiator
-- Record who started a run. A tool approval inside the run may be decided by
-- that user or by a manager of the bot, so the decision needs the run's
-- initiator as a durable fact rather than something read off a live frame.
-- The value is the principal id the admission was made under: the account id
-- when the requester has one, otherwise the channel identity id.
--
-- Nullable and additive: runs admitted before this column, and runs nobody
-- requested (schedules, subagents), have no initiator; their approvals are
-- decided by a bot manager only.

ALTER TABLE public.session_runs
  ADD COLUMN IF NOT EXISTS initiator_id TEXT;
