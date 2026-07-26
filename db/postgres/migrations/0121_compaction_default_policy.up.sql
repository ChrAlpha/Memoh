-- 0121_compaction_default_policy
-- New bots default to the model-relative compaction policy: a zero threshold
-- derives soft/hard/target levels from the chat model's context window.
-- Existing bots keep their stored absolute threshold (legacy behavior).

ALTER TABLE bots ALTER COLUMN compaction_threshold SET DEFAULT 0;
