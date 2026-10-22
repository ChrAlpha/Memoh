-- 0177_validate_history_message_admission (down)
-- Restore 0176's NOT VALID state without scanning. PostgreSQL has no
-- UNVALIDATE operation; recreate the same CHECK constraint.
ALTER TABLE bot_history_messages
  DROP CONSTRAINT IF EXISTS bot_history_messages_admission_check;
ALTER TABLE bot_history_messages
  ADD CONSTRAINT bot_history_messages_admission_check
  CHECK (admission IN ('admitted', 'rejected_acl', 'rejected_runtime_driver')) NOT VALID;
