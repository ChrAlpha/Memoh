-- 0171_history_overlay_to_checkpoints
-- Move what stands for a session's history from the compaction overlay to
-- its history checkpoint, and drop the overlay: per-row claims (compact_id),
-- summary coverage and lineage, and compaction epochs. Every session keeps
-- its context: its active summaries, with the rows in their span they left
-- raw, become its checkpoint.

-- When each bot's history started being kept under checkpoints: this
-- migration's time for the bots it finds, its creation time for every later
-- one. A context row created before it was written while a sliding window
-- decided what a request saw, so nothing guarantees that a summary covers it
-- or that a request ever rendered it.
ALTER TABLE public.bots ADD COLUMN IF NOT EXISTS history_checkpoints_since TIMESTAMPTZ NOT NULL DEFAULT now();

-- Convert each session's overlay. Only a schema that still has it holds any:
-- a fresh install builds the final schema in 0001 and the earlier
-- migrations it replays write no rows.
DO $history_checkpoints$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_catalog.pg_attribute
    WHERE attrelid = 'public.bot_history_messages'::regclass
      AND attname = 'compact_id'
      AND NOT attisdropped
  ) THEN
    RETURN;
  END IF;

  -- Every table the conversion reads or writes is under FORCE row level
  -- security keyed on memoh.team_id, which a migration connection never
  -- sets. Lift it for the conversion and restore it after, as 0165 does.
  ALTER TABLE public.bot_sessions NO FORCE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_sessions DISABLE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_messages NO FORCE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_messages DISABLE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_message_compacts NO FORCE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_message_compacts DISABLE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_checkpoints NO FORCE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_checkpoints DISABLE ROW LEVEL SECURITY;

  -- The summaries a request renders: ok, non-blank and in the session's
  -- current epoch. Each resolves to the active summary at the end of its
  -- supersede chain, the one the history load renders for its rows.
  CREATE TEMP TABLE _usable_summaries ON COMMIT DROP AS
  WITH RECURSIVE usable AS MATERIALIZED (
    SELECT compact.id, compact.team_id, compact.session_id, compact.superseded_by, compact.started_at
    FROM public.bot_history_message_compacts compact
    JOIN public.bot_sessions session
      ON session.team_id = compact.team_id
     AND session.id = compact.session_id
     AND session.bot_id = compact.bot_id
     AND session.compaction_epoch = compact.compaction_epoch
    WHERE compact.status = 'ok'
      AND NULLIF(pg_catalog.btrim(compact.summary, E' \t\n\r\f\x0B'), '') IS NOT NULL
  ), resolved AS (
    SELECT usable.id, usable.id AS active_id
    FROM usable
    WHERE usable.superseded_by IS NULL
       OR NOT EXISTS (SELECT 1 FROM usable successor WHERE successor.id = usable.superseded_by)
    UNION ALL
    SELECT parent.id, resolved.active_id
    FROM usable parent
    JOIN resolved ON parent.superseded_by = resolved.id
  )
  SELECT usable.id, usable.team_id, usable.session_id, resolved.active_id, active.started_at AS active_started_at
  FROM usable
  JOIN resolved ON resolved.id = usable.id
  JOIN usable active ON active.id = resolved.active_id;

  -- The context rows a usable summary replaces, by the active summary that
  -- renders them.
  CREATE TEMP TABLE _folded_rows ON COMMIT DROP AS
  SELECT message.id, message.team_id, message.session_id, message.turn_position, message.turn_message_seq,
    summary.active_id
  FROM public.bot_history_messages message
  JOIN _usable_summaries summary
    ON summary.team_id = message.team_id
   AND summary.id = message.compact_id
   AND summary.session_id = message.session_id
  WHERE message.turn_visible = true
    AND message.turn_id IS NOT NULL
    AND message.turn_position IS NOT NULL
    AND message.turn_message_seq IS NOT NULL
    AND (message.metadata->>'trigger_mode' IS NULL OR message.metadata->>'trigger_mode' <> 'passive_sync');
  CREATE INDEX ON _folded_rows (id);
  CREATE INDEX ON _folded_rows (session_id, turn_position DESC, turn_message_seq DESC);
  CREATE INDEX ON _folded_rows (active_id, turn_position DESC, turn_message_seq DESC);
  ANALYZE _usable_summaries;
  ANALYZE _folded_rows;

  -- The active summaries stand for every row up to the last one they
  -- replace. The rows in that span no summary replaces stay raw where they
  -- are: each summary follows the last row it replaces, and a summary none of
  -- whose rows is still visible goes first.
  WITH positioned AS (
    SELECT session.team_id, session.bot_id, session.id AS session_id,
      COALESCE(last_folded.turn_position, 0) AS turn_position,
      COALESCE(last_folded.turn_message_seq, 0) AS turn_message_seq
    FROM public.bot_sessions session
    LEFT JOIN LATERAL (
      SELECT folded.turn_position, folded.turn_message_seq
      FROM _folded_rows folded
      WHERE folded.session_id = session.id
      ORDER BY folded.turn_position DESC, folded.turn_message_seq DESC
      LIMIT 1
    ) last_folded ON true
    WHERE EXISTS (SELECT 1 FROM _usable_summaries summary WHERE summary.session_id = session.id)
  ), placed AS (
    SELECT active.session_id, active.active_id AS id,
      COALESCE(last_folded.turn_position, 0) AS turn_position,
      COALESCE(last_folded.turn_message_seq, 0) AS turn_message_seq,
      1 AS kind_order, active.active_started_at AS started_at
    FROM (SELECT DISTINCT session_id, active_id, active_started_at FROM _usable_summaries) active
    LEFT JOIN LATERAL (
      SELECT folded.turn_position, folded.turn_message_seq
      FROM _folded_rows folded
      WHERE folded.active_id = active.active_id
      ORDER BY folded.turn_position DESC, folded.turn_message_seq DESC
      LIMIT 1
    ) last_folded ON true
  ), kept AS (
    SELECT positioned.session_id, message.id, message.turn_position, message.turn_message_seq,
      0 AS kind_order, message.created_at AS started_at
    FROM positioned
    JOIN public.bot_history_messages message
      ON message.team_id = positioned.team_id
     AND message.session_id = positioned.session_id
    WHERE message.turn_visible = true
      AND message.turn_id IS NOT NULL
      AND message.turn_position IS NOT NULL
      AND message.turn_message_seq IS NOT NULL
      AND (message.metadata->>'trigger_mode' IS NULL OR message.metadata->>'trigger_mode' <> 'passive_sync')
      AND (message.turn_position, message.turn_message_seq) <= (positioned.turn_position, positioned.turn_message_seq)
      AND NOT EXISTS (SELECT 1 FROM _folded_rows folded WHERE folded.id = message.id)
  ), items AS (
    SELECT positioned.session_id,
      pg_catalog.jsonb_agg(
        CASE WHEN entry.kind_order = 0
          THEN pg_catalog.jsonb_build_object('row', entry.id)
          ELSE pg_catalog.jsonb_build_object('summary', entry.id)
        END
        ORDER BY entry.turn_position, entry.turn_message_seq, entry.kind_order, entry.started_at, entry.id
      ) AS items
    FROM positioned
    JOIN (
      SELECT session_id, id, turn_position, turn_message_seq, kind_order, started_at FROM placed
      UNION ALL
      SELECT session_id, id, turn_position, turn_message_seq, kind_order, started_at FROM kept
    ) entry ON entry.session_id = positioned.session_id
    GROUP BY positioned.session_id
  ), created AS (
    INSERT INTO public.bot_history_checkpoints (
      team_id, bot_id, session_id, turn_position, turn_message_seq, items, created_at
    )
    SELECT positioned.team_id, positioned.bot_id, positioned.session_id,
      positioned.turn_position, positioned.turn_message_seq, items.items, now()
    FROM positioned
    JOIN items ON items.session_id = positioned.session_id
    RETURNING id, session_id
  )
  UPDATE public.bot_sessions session
  SET history_checkpoint_id = created.id
  FROM created
  WHERE session.id = created.session_id;

  ALTER TABLE public.bot_history_checkpoints ENABLE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_checkpoints FORCE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_message_compacts ENABLE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_message_compacts FORCE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_messages ENABLE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_history_messages FORCE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_sessions ENABLE ROW LEVEL SECURITY;
  ALTER TABLE public.bot_sessions FORCE ROW LEVEL SECURITY;
END
$history_checkpoints$;

-- The overlay the checkpoints replace. The visible-history view projects the
-- per-row claim, so it is rebuilt without it.
DROP VIEW IF EXISTS public.bot_visible_history_messages;

ALTER TABLE public.bot_history_messages DROP CONSTRAINT IF EXISTS fk_compact_id;
DROP INDEX IF EXISTS public.idx_bot_history_messages_compact;
ALTER TABLE public.bot_history_messages DROP COLUMN IF EXISTS compact_id;

CREATE VIEW public.bot_visible_history_messages
WITH (security_invoker = true) AS
SELECT
  m.team_id,
  m.turn_id,
  m.turn_position,
  m.turn_message_seq,
  m.id,
  m.bot_id,
  m.session_id,
  m.sender_channel_identity_id,
  m.sender_account_user_id,
  m.source_message_id,
  m.source_reply_to_message_id,
  m.role,
  m.content,
  m.metadata,
  m.usage,
  m.session_mode,
  m.runtime_type,
  m.model_id,
  m.event_id,
  m.display_text,
  m.created_at
FROM public.bot_history_messages m
WHERE m.turn_visible = true
  AND m.turn_id IS NOT NULL
  AND m.turn_position IS NOT NULL
  AND m.turn_message_seq IS NOT NULL;

ALTER TABLE public.bot_history_message_compacts DROP CONSTRAINT IF EXISTS bot_history_message_compacts_superseded_by_fkey;
DROP INDEX IF EXISTS public.idx_compacts_owner_epoch;
DROP INDEX IF EXISTS public.idx_compacts_active_session;
ALTER TABLE public.bot_history_message_compacts
  DROP COLUMN IF EXISTS artifact_version,
  DROP COLUMN IF EXISTS coverage,
  DROP COLUMN IF EXISTS anchor_start_ms,
  DROP COLUMN IF EXISTS anchor_end_ms,
  DROP COLUMN IF EXISTS artifact_level,
  DROP COLUMN IF EXISTS parent_ids,
  DROP COLUMN IF EXISTS superseded_by,
  DROP COLUMN IF EXISTS superseded_at,
  DROP COLUMN IF EXISTS compaction_epoch;

ALTER TABLE public.bot_sessions DROP COLUMN IF EXISTS compaction_epoch;
