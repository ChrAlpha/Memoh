-- 0170_session_history_horizon
-- Give each session an explicit point where its agent history begins. History
-- used to start 24 hours before every request, so uncompacted rows older than
-- that silently left the context and were never summarized. Rows from the
-- horizon on now stay in the history until compaction folds them. Existing
-- sessions keep the window they had when this runs, so their first compaction
-- does not fold history they have not shown the agent for a long time; new
-- sessions start with no horizon.
--
-- bot_sessions is under FORCE row level security keyed on memoh.team_id, which
-- a migration connection never sets, so the policy is lifted for the backfill
-- and restored after, as 0165 does. The column and its backfill are added
-- together, once: a session created later has no horizon on purpose.

ALTER TABLE bot_sessions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE bot_sessions DISABLE ROW LEVEL SECURITY;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM information_schema.columns
    WHERE table_schema = 'public'
      AND table_name = 'bot_sessions'
      AND column_name = 'history_horizon_at'
  ) THEN
    ALTER TABLE bot_sessions ADD COLUMN history_horizon_at TIMESTAMPTZ;
    UPDATE bot_sessions
    SET history_horizon_at = now() - INTERVAL '24 hours'
    WHERE created_at < now() - INTERVAL '24 hours';
  END IF;
END $$;

ALTER TABLE bot_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE bot_sessions FORCE ROW LEVEL SECURITY;
