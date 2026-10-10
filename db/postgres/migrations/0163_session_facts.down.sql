-- 0163_session_facts (down)
-- Dropping the columns also drops their CHECK constraints.
ALTER TABLE public.bot_sessions
  DROP COLUMN IF EXISTS external_runtime_driver,
  DROP COLUMN IF EXISTS audience,
  DROP COLUMN IF EXISTS decision_mode,
  DROP COLUMN IF EXISTS trigger_scope,
  DROP COLUMN IF EXISTS reply_delivery;
