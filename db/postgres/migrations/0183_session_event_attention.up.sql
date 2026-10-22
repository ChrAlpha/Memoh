-- 0183_session_event_attention
-- Record on a session event whether its message mentions the bot or replies to
-- one of its messages. The discuss admission reads both for every uncovered
-- input of a session an External Agent runtime drives, to answer only what
-- addressed the bot. They were keys of event_data, which holds the whole
-- canonical event; the admission needs only these two bits for a set of rows
-- that has no upper bound.
--
-- Rows written before the columns read false, so an input that was already
-- waiting for an answer when this ran does not wake an External Agent runtime
-- in a group on its own; the next addressed message answers it with the rest.
-- ADD COLUMN with a constant default does not rewrite the table.

ALTER TABLE public.bot_session_events
    ADD COLUMN IF NOT EXISTS mentions_me BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS replies_to_me BOOLEAN NOT NULL DEFAULT false;
