-- 0180_discuss_cover_position_index
-- Index the discuss runs by the history position they covered, now that it is a
-- column. The discuss cursor of a session is the maximum cover position over
-- its completed runs, and the admission check reads it on every inbound message
-- and every sweep tick. It replaces session_runs_discuss_cover (0171), whose
-- predicate no new row satisfies; that index stays until a release may drop it.
-- This migration must remain a single statement: golang-migrate then executes
-- CREATE INDEX CONCURRENTLY outside an implicit transaction.

CREATE INDEX CONCURRENTLY IF NOT EXISTS session_runs_discuss_cover_position
    ON public.session_runs (team_id, session_id, state, discuss_cover_position)
    WHERE discuss_cover_position IS NOT NULL;
