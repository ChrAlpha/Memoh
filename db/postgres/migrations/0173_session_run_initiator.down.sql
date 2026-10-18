-- 0173_session_run_initiator (down)
ALTER TABLE public.session_runs
  DROP COLUMN IF EXISTS initiator_id;
