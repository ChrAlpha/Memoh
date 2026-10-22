-- 0181_session_runtime_owner (down)
-- The owner stays in the JSON keys this release kept writing.
ALTER TABLE public.bot_sessions
    DROP CONSTRAINT IF EXISTS bot_sessions_runtime_owner_user_id_fkey;
ALTER TABLE public.bot_sessions
    DROP COLUMN IF EXISTS runtime_owner_user_id;
