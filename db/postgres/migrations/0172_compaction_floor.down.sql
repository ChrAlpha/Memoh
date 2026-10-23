-- 0172_compaction_floor
-- Drop the compaction floor columns.

ALTER TABLE bot_history_message_compacts DROP COLUMN IF EXISTS floor_notice;
ALTER TABLE bot_history_message_compacts DROP COLUMN IF EXISTS floor_ms;
