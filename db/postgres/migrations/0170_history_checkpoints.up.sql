-- 0170_history_checkpoints
-- Persist what stands for a session's history before its context as a chain
-- of checkpoints. A checkpoint is a position in the session's turn order plus
-- what represents everything up to it: ordered items, rows kept by reference
-- and summaries. A session without an active checkpoint keeps its whole
-- history as context.

CREATE TABLE IF NOT EXISTS public.bot_history_checkpoints (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id          UUID        NOT NULL DEFAULT public.memoh_current_team_id()
                                 REFERENCES public.teams(id) ON DELETE RESTRICT,
    bot_id           UUID        NOT NULL,
    session_id       UUID        NOT NULL,
    parent_id        UUID,
    turn_position    BIGINT      NOT NULL,
    turn_message_seq BIGINT      NOT NULL,
    items            JSONB       NOT NULL DEFAULT '[]'::jsonb,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT memoh_team_key_c3970c8700e1 UNIQUE (team_id, id),
    CONSTRAINT bot_history_checkpoints_position_check CHECK (turn_position >= 0 AND turn_message_seq >= 0),
    CONSTRAINT bot_history_checkpoints_items_check CHECK (pg_catalog.jsonb_typeof(items) = 'array'),
    CONSTRAINT bot_history_checkpoints_summary_check CHECK (pg_catalog.jsonb_path_exists(items, '$[*].summary')),
    CONSTRAINT bot_history_checkpoints_bot_id_fkey
        FOREIGN KEY (team_id, bot_id)
        REFERENCES public.bots(team_id, id) ON DELETE CASCADE,
    CONSTRAINT bot_history_checkpoints_session_id_fkey
        FOREIGN KEY (team_id, session_id)
        REFERENCES public.bot_sessions(team_id, id) ON DELETE CASCADE,
    CONSTRAINT bot_history_checkpoints_parent_id_fkey
        FOREIGN KEY (team_id, parent_id)
        REFERENCES public.bot_history_checkpoints(team_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_bot_history_checkpoints_session
    ON public.bot_history_checkpoints (team_id, session_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_bot_history_checkpoints_parent
    ON public.bot_history_checkpoints (team_id, parent_id)
    WHERE parent_id IS NOT NULL;

ALTER TABLE public.bot_history_checkpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.bot_history_checkpoints FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS bot_history_checkpoints_team_select ON public.bot_history_checkpoints;
CREATE POLICY bot_history_checkpoints_team_select ON public.bot_history_checkpoints
    FOR SELECT USING (team_id = public.memoh_current_team_id());
DROP POLICY IF EXISTS bot_history_checkpoints_team_insert ON public.bot_history_checkpoints;
CREATE POLICY bot_history_checkpoints_team_insert ON public.bot_history_checkpoints
    FOR INSERT WITH CHECK (team_id = public.memoh_current_team_id());
DROP POLICY IF EXISTS bot_history_checkpoints_team_update ON public.bot_history_checkpoints;
CREATE POLICY bot_history_checkpoints_team_update ON public.bot_history_checkpoints
    FOR UPDATE
    USING (team_id = public.memoh_current_team_id())
    WITH CHECK (team_id = public.memoh_current_team_id());
DROP POLICY IF EXISTS bot_history_checkpoints_team_delete ON public.bot_history_checkpoints;
CREATE POLICY bot_history_checkpoints_team_delete ON public.bot_history_checkpoints
    FOR DELETE USING (team_id = public.memoh_current_team_id());

-- The session's active checkpoint. NULL: nothing stands for any of its
-- history, so its whole history is context.
ALTER TABLE public.bot_sessions ADD COLUMN IF NOT EXISTS history_checkpoint_id UUID;
-- Validating the reference scans both tables, which are under FORCE row
-- level security keyed on memoh.team_id, which a migration connection never
-- sets. Lift it around the validation and restore it after.
DO $history_checkpoint_fkey$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_catalog.pg_constraint
    WHERE conrelid = 'public.bot_sessions'::regclass
      AND conname = 'bot_sessions_history_checkpoint_id_fkey'
  ) THEN
    ALTER TABLE public.bot_sessions NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE public.bot_sessions DISABLE ROW LEVEL SECURITY;
    ALTER TABLE public.bot_history_checkpoints NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE public.bot_history_checkpoints DISABLE ROW LEVEL SECURITY;
    ALTER TABLE public.bot_sessions
      ADD CONSTRAINT bot_sessions_history_checkpoint_id_fkey
      FOREIGN KEY (team_id, history_checkpoint_id)
      REFERENCES public.bot_history_checkpoints(team_id, id)
      ON DELETE SET NULL (history_checkpoint_id);
    ALTER TABLE public.bot_history_checkpoints ENABLE ROW LEVEL SECURITY;
    ALTER TABLE public.bot_history_checkpoints FORCE ROW LEVEL SECURITY;
    ALTER TABLE public.bot_sessions ENABLE ROW LEVEL SECURITY;
    ALTER TABLE public.bot_sessions FORCE ROW LEVEL SECURITY;
  END IF;
END
$history_checkpoint_fkey$;

-- A context row that appeared at or before the position of the checkpoint
-- its session had active then, checkpoint_id: that checkpoint does not
-- account for the row, and neither do its ancestors that reach it. Written
-- by memoh_history_row_appeared; the next read of the session's checkpoint
-- moves the session back before the row and forgets it.
CREATE TABLE IF NOT EXISTS public.bot_history_appeared_rows (
    message_id       UUID        PRIMARY KEY,
    team_id          UUID        NOT NULL DEFAULT public.memoh_current_team_id()
                                 REFERENCES public.teams(id) ON DELETE RESTRICT,
    session_id       UUID        NOT NULL,
    checkpoint_id    UUID        NOT NULL,
    turn_position    BIGINT      NOT NULL,
    turn_message_seq BIGINT      NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT memoh_team_key_4b672c98f5bf UNIQUE (team_id, message_id),
    CONSTRAINT bot_history_appeared_rows_message_id_fkey
        FOREIGN KEY (team_id, message_id)
        REFERENCES public.bot_history_messages(team_id, id) ON DELETE CASCADE,
    CONSTRAINT bot_history_appeared_rows_session_id_fkey
        FOREIGN KEY (team_id, session_id)
        REFERENCES public.bot_sessions(team_id, id) ON DELETE CASCADE,
    CONSTRAINT bot_history_appeared_rows_checkpoint_id_fkey
        FOREIGN KEY (team_id, checkpoint_id)
        REFERENCES public.bot_history_checkpoints(team_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_bot_history_appeared_rows_session
    ON public.bot_history_appeared_rows (team_id, session_id);
CREATE INDEX IF NOT EXISTS idx_bot_history_appeared_rows_checkpoint
    ON public.bot_history_appeared_rows (team_id, checkpoint_id);

ALTER TABLE public.bot_history_appeared_rows ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.bot_history_appeared_rows FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS bot_history_appeared_rows_team_select ON public.bot_history_appeared_rows;
CREATE POLICY bot_history_appeared_rows_team_select ON public.bot_history_appeared_rows
    FOR SELECT USING (team_id = public.memoh_current_team_id());
DROP POLICY IF EXISTS bot_history_appeared_rows_team_insert ON public.bot_history_appeared_rows;
CREATE POLICY bot_history_appeared_rows_team_insert ON public.bot_history_appeared_rows
    FOR INSERT WITH CHECK (team_id = public.memoh_current_team_id());
DROP POLICY IF EXISTS bot_history_appeared_rows_team_update ON public.bot_history_appeared_rows;
CREATE POLICY bot_history_appeared_rows_team_update ON public.bot_history_appeared_rows
    FOR UPDATE
    USING (team_id = public.memoh_current_team_id())
    WITH CHECK (team_id = public.memoh_current_team_id());
DROP POLICY IF EXISTS bot_history_appeared_rows_team_delete ON public.bot_history_appeared_rows;
CREATE POLICY bot_history_appeared_rows_team_delete ON public.bot_history_appeared_rows
    FOR DELETE USING (team_id = public.memoh_current_team_id());

-- memoh_history_checkpoint_kept is the newest checkpoint on a session's active
-- chain that still holds once rows from a position on were removed (deleted,
-- hidden or replaced). Its summary may have read any row up to its position,
-- so a checkpoint holds only when it is before the position. NULL when no
-- checkpoint holds.
CREATE OR REPLACE FUNCTION public.memoh_history_checkpoint_kept(
  p_session_id uuid,
  p_turn_position bigint,
  p_turn_message_seq bigint
)
RETURNS uuid
LANGUAGE sql
STABLE
SECURITY INVOKER
SET search_path = pg_catalog, pg_temp
AS $$
  WITH RECURSIVE chain AS (
    SELECT checkpoint.id, checkpoint.parent_id, checkpoint.turn_position, checkpoint.turn_message_seq,
      0 AS depth
    FROM public.bot_sessions session
    JOIN public.bot_history_checkpoints checkpoint
      ON checkpoint.team_id = session.team_id
     AND checkpoint.id = session.history_checkpoint_id
    WHERE session.team_id = public.memoh_current_team_id()
      AND session.id = p_session_id
    UNION ALL
    SELECT parent.id, parent.parent_id, parent.turn_position, parent.turn_message_seq,
      chain.depth + 1
    FROM chain
    JOIN public.bot_history_checkpoints parent
      ON parent.team_id = public.memoh_current_team_id()
     AND parent.id = chain.parent_id
  )
  SELECT chain.id
  FROM chain
  WHERE (chain.turn_position, chain.turn_message_seq) < (p_turn_position, p_turn_message_seq)
  ORDER BY chain.depth
  LIMIT 1
$$;

-- memoh_rewind_history_checkpoint makes the newest checkpoint that still
-- holds once rows of the session from a position on were removed, as
-- memoh_history_checkpoint_kept reads the removal, the session's active one,
-- and reports whether that moved it. The caller holds the session row lock.
CREATE OR REPLACE FUNCTION public.memoh_rewind_history_checkpoint(
  p_session_id uuid,
  p_turn_position bigint,
  p_turn_message_seq bigint
)
RETURNS boolean
LANGUAGE plpgsql
VOLATILE
SECURITY INVOKER
SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
  active uuid;
  kept uuid;
BEGIN
  SELECT session.history_checkpoint_id
    INTO active
  FROM public.bot_sessions session
  WHERE session.team_id = public.memoh_current_team_id()
    AND session.id = p_session_id;
  IF active IS NULL THEN
    RETURN false;
  END IF;
  kept := public.memoh_history_checkpoint_kept(p_session_id, p_turn_position, p_turn_message_seq);
  IF kept IS NOT DISTINCT FROM active THEN
    RETURN false;
  END IF;
  UPDATE public.bot_sessions session
  SET history_checkpoint_id = kept
  WHERE session.team_id = public.memoh_current_team_id()
    AND session.id = p_session_id;
  RETURN true;
END;
$$;

-- memoh_history_checkpoint_hides reports whether a checkpoint hides a context
-- row that appeared at a position at or before that of p_appeared_below, the
-- checkpoint its session had active then: whether it is that checkpoint or
-- one of its ancestors that reaches the position, whose summary may stand for
-- any row up to it. A checkpoint made after it read the row. Positions only
-- grow along a chain, so the walk stops below the row.
CREATE OR REPLACE FUNCTION public.memoh_history_checkpoint_hides(
  p_checkpoint_id uuid,
  p_appeared_below uuid,
  p_turn_position bigint,
  p_turn_message_seq bigint
)
RETURNS boolean
LANGUAGE sql
STABLE
SECURITY INVOKER
SET search_path = pg_catalog, pg_temp
AS $$
  WITH RECURSIVE chain AS (
    SELECT checkpoint.id, checkpoint.parent_id
    FROM public.bot_history_checkpoints checkpoint
    WHERE checkpoint.team_id = public.memoh_current_team_id()
      AND checkpoint.id = p_appeared_below
      AND (checkpoint.turn_position, checkpoint.turn_message_seq) >= (p_turn_position, p_turn_message_seq)
    UNION ALL
    SELECT parent.id, parent.parent_id
    FROM chain
    JOIN public.bot_history_checkpoints parent
      ON parent.team_id = public.memoh_current_team_id()
     AND parent.id = chain.parent_id
    WHERE (parent.turn_position, parent.turn_message_seq) >= (p_turn_position, p_turn_message_seq)
  )
  SELECT EXISTS (SELECT 1 FROM chain WHERE chain.id = p_checkpoint_id)
$$;

-- memoh_history_row_appeared records a context row that appears at or before
-- the position of its session's active checkpoint, unless that checkpoint
-- keeps the row by reference: nothing it holds accounts for the row, which
-- would otherwise stay hidden behind it (bot_history_appeared_rows). A
-- context row is a visible row of a turn, other than a legacy passive_sync
-- row. The writer holds the session's key share, through the row's foreign
-- key or taken here, and a fold commits under the session's row lock, so
-- every commit either sees the row or is seen here. The writer takes no
-- stronger lock on the session: it would wait on a transaction that holds
-- the session's row lock before it locks the session for update, which
-- waits on this key share.
CREATE OR REPLACE FUNCTION public.memoh_history_row_appeared()
RETURNS trigger
LANGUAGE plpgsql
VOLATILE
SECURITY INVOKER
SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
  PERFORM 1
  FROM public.bot_sessions session
  WHERE session.team_id = NEW.team_id
    AND session.id = NEW.session_id
  FOR KEY SHARE;
  INSERT INTO public.bot_history_appeared_rows (
    message_id, team_id, session_id, checkpoint_id, turn_position, turn_message_seq
  )
  SELECT NEW.id, NEW.team_id, NEW.session_id, checkpoint.id, NEW.turn_position, NEW.turn_message_seq
  FROM public.bot_sessions session
  JOIN public.bot_history_checkpoints checkpoint
    ON checkpoint.team_id = session.team_id
   AND checkpoint.id = session.history_checkpoint_id
  WHERE session.team_id = NEW.team_id
    AND session.id = NEW.session_id
    AND (checkpoint.turn_position, checkpoint.turn_message_seq) >= (NEW.turn_position, NEW.turn_message_seq)
    AND NOT checkpoint.items @> pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_object('row', NEW.id))
  ON CONFLICT (message_id) DO UPDATE
  SET session_id = EXCLUDED.session_id,
    checkpoint_id = EXCLUDED.checkpoint_id,
    turn_position = EXCLUDED.turn_position,
    turn_message_seq = EXCLUDED.turn_message_seq,
    created_at = EXCLUDED.created_at;
  RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS bot_history_messages_appear ON public.bot_history_messages;
CREATE TRIGGER bot_history_messages_appear
  AFTER INSERT ON public.bot_history_messages
  FOR EACH ROW
  WHEN (
    NEW.turn_visible
    AND NEW.session_id IS NOT NULL
    AND NEW.turn_id IS NOT NULL
    AND NEW.turn_position IS NOT NULL
    AND NEW.turn_message_seq IS NOT NULL
    AND (NEW.metadata->>'trigger_mode') IS DISTINCT FROM 'passive_sync'
  )
  EXECUTE FUNCTION public.memoh_history_row_appeared();

DROP TRIGGER IF EXISTS bot_history_messages_reappear ON public.bot_history_messages;
CREATE TRIGGER bot_history_messages_reappear
  AFTER UPDATE OF turn_visible, turn_id, turn_position, turn_message_seq, session_id, metadata
  ON public.bot_history_messages
  FOR EACH ROW
  WHEN (
    NEW.turn_visible
    AND NEW.session_id IS NOT NULL
    AND NEW.turn_id IS NOT NULL
    AND NEW.turn_position IS NOT NULL
    AND NEW.turn_message_seq IS NOT NULL
    AND (NEW.metadata->>'trigger_mode') IS DISTINCT FROM 'passive_sync'
    AND NOT (
      OLD.turn_visible
      AND OLD.turn_id IS NOT NULL
      AND (OLD.metadata->>'trigger_mode') IS DISTINCT FROM 'passive_sync'
      AND OLD.session_id IS NOT DISTINCT FROM NEW.session_id
      AND OLD.turn_position IS NOT DISTINCT FROM NEW.turn_position
      AND OLD.turn_message_seq IS NOT DISTINCT FROM NEW.turn_message_seq
    )
  )
  EXECUTE FUNCTION public.memoh_history_row_appeared();
