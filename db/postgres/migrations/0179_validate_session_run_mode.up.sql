-- 0179_validate_session_run_mode
-- Separate from 0178: VALIDATE takes SHARE UPDATE EXCLUSIVE, allowing normal
-- reads and writes while scanning. No DROP/ADD belongs in this transaction.
ALTER TABLE public.session_runs VALIDATE CONSTRAINT session_runs_mode_check;
ALTER TABLE public.session_runs VALIDATE CONSTRAINT session_runs_discuss_cover_position_check;
ALTER TABLE public.session_runs VALIDATE CONSTRAINT session_runs_discuss_cover_pairing_check;
