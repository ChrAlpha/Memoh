-- 0178_session_run_mode (down)
-- Discuss runs lose their cover position with the column; the discuss cursor
-- of a release that reads input_json starts again from what that key holds.
ALTER TABLE public.session_runs
    DROP CONSTRAINT IF EXISTS session_runs_discuss_cover_pairing_check;
ALTER TABLE public.session_runs
    DROP CONSTRAINT IF EXISTS session_runs_discuss_cover_position_check;
ALTER TABLE public.session_runs
    DROP CONSTRAINT IF EXISTS session_runs_mode_check;
ALTER TABLE public.session_runs
    DROP COLUMN IF EXISTS discuss_cover_position,
    DROP COLUMN IF EXISTS mode;
