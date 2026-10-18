-- 0169_session_facts
-- Record on each session the facts session_mode used to stand in for: whether
-- ordinary model text reaches the conversation (reply_delivery), what starts
-- and what one run covers (trigger_scope), whether a run may park on a
-- decision (decision_mode), whether the conversation is personal or shared
-- (audience), and who may drive the runtime owner's external runtime
-- (external_runtime_driver). session_mode stays as the preset name and label.
--
-- Column defaults are the chat preset, which every non-chat row is then
-- rewritten from. Audience comes from the route the session was created on:
-- a group or thread conversation is shared; private, Web and routeless
-- sessions are personal. The conversation type is normalized the same way
-- the channel layer does (empty, p2p, direct and private are private).

ALTER TABLE public.bot_sessions
  ADD COLUMN IF NOT EXISTS reply_delivery TEXT NOT NULL DEFAULT 'stream'
    CHECK (reply_delivery IN ('stream', 'explicit')),
  ADD COLUMN IF NOT EXISTS trigger_scope TEXT NOT NULL DEFAULT 'addressed_message'
    CHECK (trigger_scope IN ('addressed_message', 'uncovered_events')),
  ADD COLUMN IF NOT EXISTS decision_mode TEXT NOT NULL DEFAULT 'park'
    CHECK (decision_mode IN ('park', 'auto_reject')),
  ADD COLUMN IF NOT EXISTS audience TEXT NOT NULL DEFAULT 'personal'
    CHECK (audience IN ('personal', 'shared')),
  ADD COLUMN IF NOT EXISTS external_runtime_driver TEXT NOT NULL DEFAULT 'owner_only'
    CHECK (external_runtime_driver IN ('owner_only', 'participants'));

-- These tables use FORCE RLS keyed on memoh.team_id, which a migration
-- connection never sets. Lift the policies for the backfill and restore them
-- before leaving the migration.
ALTER TABLE public.bot_sessions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.bot_sessions DISABLE ROW LEVEL SECURITY;
ALTER TABLE public.bot_channel_routes NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.bot_channel_routes DISABLE ROW LEVEL SECURITY;

UPDATE public.bot_sessions
SET reply_delivery = 'explicit',
    trigger_scope = 'uncovered_events',
    decision_mode = 'auto_reject',
    external_runtime_driver = 'participants'
WHERE session_mode = 'discuss';

UPDATE public.bot_sessions
SET decision_mode = 'auto_reject'
WHERE session_mode IN ('schedule', 'subagent');

UPDATE public.bot_sessions session
SET audience = 'shared'
FROM public.bot_channel_routes route
WHERE route.id = session.route_id
  AND lower(btrim(route.channel_type)) NOT IN ('web', 'cli', 'local')
  AND lower(btrim(COALESCE(route.conversation_type, ''))) NOT IN ('', 'p2p', 'direct', 'private');

ALTER TABLE public.bot_channel_routes ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.bot_channel_routes FORCE ROW LEVEL SECURITY;
ALTER TABLE public.bot_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.bot_sessions FORCE ROW LEVEL SECURITY;
