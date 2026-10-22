-- 0181_session_runtime_owner
-- Record the account an External Agent session acts for as a column. Sessions
-- that an ACP or direct runtime drives act for their runtime owner: the route
-- credentials issued for a turn name it, and a missing owner means no
-- credentials, not the sender's. The owner lived in two JSON keys
-- (metadata and runtime_metadata) of which clearing the history removed one,
-- and the discuss admission read only that one.
--
-- NULL is no owner: the session predates the column and has not been read
-- since, or its owner left the team. The foreign key follows the team schema:
-- a member removed from the team stops being an owner (SET NULL on the column
-- only). team_members is under FORCE ROW LEVEL SECURITY, so the reference is
-- added NOT VALID and never validated through a policy-scoped scan, as the
-- other references to such tables are; it still holds for every row written
-- from now on. 0182 indexes the column.
--
-- This release keeps writing the JSON keys for images that still read them.
-- Sessions created before the column get it from the first read that finds an
-- owner in metadata, and from a background pass that fills the rest.

ALTER TABLE public.bot_sessions
    ADD COLUMN IF NOT EXISTS runtime_owner_user_id UUID;

ALTER TABLE public.bot_sessions
    DROP CONSTRAINT IF EXISTS bot_sessions_runtime_owner_user_id_fkey;
ALTER TABLE public.bot_sessions
    ADD CONSTRAINT bot_sessions_runtime_owner_user_id_fkey
    FOREIGN KEY (team_id, runtime_owner_user_id)
    REFERENCES public.team_members(team_id, user_id)
    ON DELETE SET NULL (runtime_owner_user_id) NOT VALID;
