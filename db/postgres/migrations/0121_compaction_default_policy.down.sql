-- 0121_compaction_default_policy
-- Restore the legacy absolute compaction threshold default.

ALTER TABLE bots ALTER COLUMN compaction_threshold SET DEFAULT 100000;
