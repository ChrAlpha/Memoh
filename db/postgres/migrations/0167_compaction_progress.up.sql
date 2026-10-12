-- 0167_compaction_progress
-- Resume bounded compaction scans without treating skipped history as covered.
ALTER TABLE bot_sessions
  ADD COLUMN IF NOT EXISTS compaction_scan_after UUID,
  ADD COLUMN IF NOT EXISTS compaction_scan_epoch BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS compaction_scan_at TIMESTAMPTZ;
