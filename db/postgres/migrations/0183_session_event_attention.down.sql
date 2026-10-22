-- 0183_session_event_attention (down)
-- The flags stay in event_data, which this release kept writing.
ALTER TABLE public.bot_session_events
    DROP COLUMN IF EXISTS replies_to_me,
    DROP COLUMN IF EXISTS mentions_me;
