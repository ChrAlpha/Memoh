-- 0172_compaction_floor
-- A summary can stand on a floor: the rows created up to floor_ms are left out
-- of the request without being summarized, and floor_notice, which opens the
-- summary, tells the model so. A thread whose backlog predates rolling
-- summaries gets a floor once, so compacting it folds only what its last
-- request read and what came after; every summary that folds a floored one
-- inherits it.

ALTER TABLE bot_history_message_compacts
  ADD COLUMN IF NOT EXISTS floor_ms BIGINT NOT NULL DEFAULT 0;
ALTER TABLE bot_history_message_compacts
  ADD COLUMN IF NOT EXISTS floor_notice TEXT NOT NULL DEFAULT '';
