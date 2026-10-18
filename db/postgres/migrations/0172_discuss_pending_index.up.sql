-- 0172_discuss_pending_index
-- Index the passive external inputs a discuss session may have to answer. The
-- admission check looks for user rows that were written with an event and sit
-- past the session's cover position. Bot output is stored as assistant rows, so
-- the role filter also excludes the bot's own messages. Rows refused at
-- admission carry no event and never appear in this index.
-- This migration must remain a single statement: golang-migrate then executes
-- CREATE INDEX CONCURRENTLY outside an implicit transaction.

CREATE INDEX CONCURRENTLY IF NOT EXISTS bot_history_messages_discuss_pending
    ON public.bot_history_messages (team_id, session_id, turn_position)
    WHERE event_id IS NOT NULL AND role = 'user';
