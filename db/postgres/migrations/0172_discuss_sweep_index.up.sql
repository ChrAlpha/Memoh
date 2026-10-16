-- 0172_discuss_sweep_index
-- Index the external inputs by arrival time. The discuss sweep looks for sessions
-- that took external input within its window, once a minute, and must not walk
-- a team's whole history to find them. The index keeps the same rows as
-- bot_history_messages_discuss_pending.
-- This migration must remain a single statement: golang-migrate then executes
-- CREATE INDEX CONCURRENTLY outside an implicit transaction.

CREATE INDEX CONCURRENTLY IF NOT EXISTS bot_history_messages_discuss_recent
    ON public.bot_history_messages (team_id, created_at)
    WHERE event_id IS NOT NULL AND role = 'user';
