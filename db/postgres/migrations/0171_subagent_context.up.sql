-- 0171_subagent_context
-- Give child handoff rows the same checkpoint read contract as ordinary history.
-- Model context includes inherited child material without exposing it as chat.
-- Disjoint UNION ALL arms retain the visible-history and fork-context indexes.
CREATE OR REPLACE VIEW public.bot_context_history_messages
WITH (security_invoker = true) AS
SELECT m.* FROM public.bot_history_messages m
WHERE m.turn_visible = true
  AND m.turn_id IS NOT NULL AND m.turn_position IS NOT NULL AND m.turn_message_seq IS NOT NULL
UNION ALL
SELECT m.* FROM public.bot_history_messages m
WHERE m.turn_visible = false
  AND m.session_mode = 'subagent'
  AND m.metadata->>'context_scope' = 'subagent_fork'
  AND m.turn_id IS NOT NULL AND m.turn_position IS NOT NULL AND m.turn_message_seq IS NOT NULL;
