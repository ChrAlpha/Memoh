-- 0179_validate_session_run_mode (down)
-- Restore 0178's NOT VALID state without scanning. PostgreSQL has no
-- UNVALIDATE operation; recreate the same CHECK constraints.
ALTER TABLE public.session_runs
    DROP CONSTRAINT IF EXISTS session_runs_mode_check;
ALTER TABLE public.session_runs
    ADD CONSTRAINT session_runs_mode_check
    CHECK (mode IN ('chat', 'schedule', 'subagent', 'discuss', 'acp_agent')) NOT VALID;

ALTER TABLE public.session_runs
    DROP CONSTRAINT IF EXISTS session_runs_discuss_cover_position_check;
ALTER TABLE public.session_runs
    ADD CONSTRAINT session_runs_discuss_cover_position_check
    CHECK (discuss_cover_position > 0) NOT VALID;

ALTER TABLE public.session_runs
    DROP CONSTRAINT IF EXISTS session_runs_discuss_cover_pairing_check;
ALTER TABLE public.session_runs
    ADD CONSTRAINT session_runs_discuss_cover_pairing_check
    CHECK ((mode = 'discuss') = (discuss_cover_position IS NOT NULL)) NOT VALID;
