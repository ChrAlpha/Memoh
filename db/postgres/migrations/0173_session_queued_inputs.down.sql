-- 0173_session_queued_inputs (down)
-- Drop the session input queue. Inputs still waiting are lost.

DROP TABLE IF EXISTS public.session_queued_inputs;
