-- 0181_session_runtime_owner
-- Record the account an External Agent session acts for as a column. Sessions
-- that an ACP or direct runtime drives act for their runtime owner: the route
-- credentials issued for a turn name it, and a missing owner means no
-- credentials, not the sender's. The owner lived in two JSON keys
-- (metadata and runtime_metadata) of which clearing the history removed one,
-- and the discuss admission read only that one.
--
-- NULL is no owner: the session's metadata names nobody who is a member of its
-- team, or its owner left the team. The foreign key follows the team schema:
-- a member removed from the team stops being an owner (SET NULL on the column
-- only). team_members is under FORCE ROW LEVEL SECURITY, so the reference is
-- added NOT VALID and never validated through a policy-scoped scan, as the
-- other references to such tables are; it still holds for every row written
-- from now on. 0182 indexes the column.
--
-- This release keeps writing the JSON keys for images that still read them.
-- Existing sessions get the column here, once, from the key in metadata (clearing
-- the history leaves that copy alone, unlike the one in runtime_metadata). This
-- is the one data move the migrations make, because a column nothing fills
-- leaves every session created before it without credentials. A session created
-- by an image that does not know the column, between this migration and the new
-- images starting, keeps NULL until it is bound again.

ALTER TABLE public.bot_sessions
    ADD COLUMN IF NOT EXISTS runtime_owner_user_id UUID;

ALTER TABLE public.bot_sessions
    DROP CONSTRAINT IF EXISTS bot_sessions_runtime_owner_user_id_fkey;
ALTER TABLE public.bot_sessions
    ADD CONSTRAINT bot_sessions_runtime_owner_user_id_fkey
    FOREIGN KEY (team_id, runtime_owner_user_id)
    REFERENCES public.team_members(team_id, user_id)
    ON DELETE SET NULL (runtime_owner_user_id) NOT VALID;

-- bot_sessions and team_members use FORCE RLS keyed on memoh.team_id, which a
-- migration connection never sets. Lift the policies for the fill and restore
-- them before leaving the migration, as 0169 does. Matching on the text of the
-- member's id skips any value that is not an account id without casting it.
ALTER TABLE public.bot_sessions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.bot_sessions DISABLE ROW LEVEL SECURITY;
ALTER TABLE public.team_members NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.team_members DISABLE ROW LEVEL SECURITY;

UPDATE public.bot_sessions s
SET runtime_owner_user_id = m.user_id
FROM public.team_members m
WHERE m.team_id = s.team_id
  AND m.user_id::text = lower(btrim(s.metadata->>'runtime_owner_account_id'))
  AND s.runtime_type <> 'model'
  AND s.runtime_owner_user_id IS NULL;

ALTER TABLE public.team_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.team_members FORCE ROW LEVEL SECURITY;
ALTER TABLE public.bot_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.bot_sessions FORCE ROW LEVEL SECURITY;
