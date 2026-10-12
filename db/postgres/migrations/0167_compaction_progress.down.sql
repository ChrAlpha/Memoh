-- 0167_compaction_progress
ALTER TABLE bot_sessions
  DROP COLUMN IF EXISTS compaction_scan_at,
  DROP COLUMN IF EXISTS compaction_scan_epoch,
  DROP COLUMN IF EXISTS compaction_scan_after;
