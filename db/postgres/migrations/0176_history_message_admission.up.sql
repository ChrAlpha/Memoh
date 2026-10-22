-- 0176_history_message_admission
-- Record at the door whether a history row may reach a model, as a column.
-- The ACL and the external runtime driver gate refuse some channel rows; those
-- rows stay in history for display but never enter context, memory queries,
-- compaction input or history tool results. Every reader serving a model
-- filters on this column, so the fact is stated once and a mistyped value is
-- refused at write time instead of silently reading as admitted.
--
-- Existing rows take the default: before this column nothing marked a row
-- refused in a database that could hold one. The CHECK is added NOT VALID so
-- this transaction does not scan the table; 0177 validates it.

ALTER TABLE bot_history_messages
  ADD COLUMN IF NOT EXISTS admission TEXT NOT NULL DEFAULT 'admitted';

ALTER TABLE bot_history_messages
  DROP CONSTRAINT IF EXISTS bot_history_messages_admission_check;
ALTER TABLE bot_history_messages
  ADD CONSTRAINT bot_history_messages_admission_check
  CHECK (admission IN ('admitted', 'rejected_acl', 'rejected_runtime_driver')) NOT VALID;

-- The visible-history view serves the model readers, so it carries the column.
-- A column appended at the end is what CREATE OR REPLACE VIEW accepts.
CREATE OR REPLACE VIEW bot_visible_history_messages
WITH (security_invoker = true) AS
SELECT
  m.team_id,
  m.turn_id,
  m.turn_position,
  m.turn_message_seq,
  m.id,
  m.bot_id,
  m.session_id,
  m.sender_channel_identity_id,
  m.sender_account_user_id,
  m.source_message_id,
  m.source_reply_to_message_id,
  m.role,
  m.content,
  m.metadata,
  m.usage,
  m.compact_id,
  m.session_mode,
  m.runtime_type,
  m.model_id,
  m.event_id,
  m.display_text,
  m.created_at,
  m.admission
FROM bot_history_messages m
WHERE m.turn_visible = true
  AND m.turn_id IS NOT NULL
  AND m.turn_position IS NOT NULL
  AND m.turn_message_seq IS NOT NULL;
