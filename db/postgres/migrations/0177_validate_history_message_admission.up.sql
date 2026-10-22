-- 0177_validate_history_message_admission
-- Separate from 0176: VALIDATE takes SHARE UPDATE EXCLUSIVE, allowing normal
-- reads and writes while scanning. No DROP/ADD belongs in this transaction.
ALTER TABLE bot_history_messages VALIDATE CONSTRAINT bot_history_messages_admission_check;
