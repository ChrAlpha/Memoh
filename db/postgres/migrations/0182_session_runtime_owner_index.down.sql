-- 0182_session_runtime_owner_index (down)
DROP INDEX CONCURRENTLY IF EXISTS public.idx_bot_sessions_runtime_owner_user_id;
