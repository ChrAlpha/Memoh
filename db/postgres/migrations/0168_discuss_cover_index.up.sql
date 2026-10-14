-- 0168_discuss_cover_index
-- Index the discuss runs by the history position they covered. The discuss
-- cursor of a session is the maximum cover_position over its completed runs,
-- and the admission check reads it on every inbound message and every sweep
-- tick. The partial index keeps only runs that carry a cover_position, so the
-- lookup does not walk a team's run history.
-- This migration must remain a single statement: golang-migrate then executes
-- CREATE INDEX CONCURRENTLY outside an implicit transaction.

CREATE INDEX CONCURRENTLY IF NOT EXISTS session_runs_discuss_cover
    ON public.session_runs (team_id, session_id, state, ((input_json->>'cover_position')::bigint))
    WHERE input_json ? 'cover_position';
