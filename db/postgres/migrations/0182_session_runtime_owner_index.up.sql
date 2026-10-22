-- 0182_session_runtime_owner_index
-- Index the runtime owner so removing a team member finds the sessions whose
-- owner column it clears without walking the table.
-- This migration must remain a single statement: golang-migrate then executes
-- CREATE INDEX CONCURRENTLY outside an implicit transaction.

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_bot_sessions_runtime_owner_user_id
    ON public.bot_sessions (team_id, runtime_owner_user_id)
    WHERE runtime_owner_user_id IS NOT NULL;
