-- 0124_compaction_target_percent
-- Replace the legacy compaction ratio with a nullable window keep-share override.

ALTER TABLE bots
  ADD COLUMN IF NOT EXISTS compaction_target_percent INTEGER;

DO $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM pg_attribute
    WHERE attrelid = 'bots'::regclass
      AND attname = 'compaction_ratio'
      AND NOT attisdropped
  ) THEN
    UPDATE bots
    SET compaction_target_percent = 100 - compaction_ratio
    WHERE compaction_threshold > 0
      AND compaction_target_percent IS NULL;
  END IF;
END
$$;

ALTER TABLE bots
  DROP COLUMN IF EXISTS compaction_ratio;
