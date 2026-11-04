-- 0174_history_checkpoint_kept_stops_early
-- Restore the walk over the whole checkpoint chain.

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
