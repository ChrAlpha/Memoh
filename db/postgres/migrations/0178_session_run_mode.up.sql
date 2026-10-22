-- 0178_session_run_mode
-- Record what kind of run a session run is, and for a discuss run the batch it
-- answers, as columns. The discuss admission decides from them whether an
-- input is already answered (the cover position cursor), whether a pending
-- input was taken by a non-discuss run, and which sessions the sweep revisits.
-- Those decisions used to read keys of input_json, where a missing key meant
-- "not a discuss run" and a misspelt key was indistinguishable from it.
--
-- mode is the session mode the run was admitted in. A discuss run covers a
-- history position, and only a discuss run does: the third CHECK states that
-- pairing so the two facts cannot disagree.
--
-- Existing rows take mode 'chat' and no cover position, which satisfies the
-- CHECKs. A discuss run admitted by an image that predates this migration
-- lands there too, so its batch is not counted as covered and may be answered
-- again; it is never skipped. The CHECKs are added NOT VALID so this
-- transaction does not scan the table; 0179 validates them. The index that
-- serves the cursor is 0180.

ALTER TABLE public.session_runs
    ADD COLUMN IF NOT EXISTS mode TEXT NOT NULL DEFAULT 'chat',
    ADD COLUMN IF NOT EXISTS discuss_cover_position BIGINT;

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
