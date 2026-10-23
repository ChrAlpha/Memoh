-- 0170_session_history_horizon
-- Drop the session history horizon.

ALTER TABLE bot_sessions DROP COLUMN IF EXISTS history_horizon_at;
