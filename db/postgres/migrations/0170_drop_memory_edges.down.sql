-- 0170_drop_memory_edges
-- Restore the memory_edges schema. Its rows were derived from memory_nodes and
-- are not restored; no reader remains for them.

CREATE TABLE IF NOT EXISTS public.memory_edges (
    id          BIGSERIAL    PRIMARY KEY,
    bot_id      UUID         NOT NULL,
    src_node    TEXT         NOT NULL,
    dst_node    TEXT         NOT NULL,
    rel         TEXT         NOT NULL,
    weight      REAL         NOT NULL DEFAULT 1.0,
    metadata    JSONB        NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    team_id     UUID         NOT NULL DEFAULT public.memoh_current_team_id(),
    CONSTRAINT memoh_team_key_483f14eea5aa UNIQUE (team_id, id),
    CONSTRAINT memory_edges_unique UNIQUE (team_id, bot_id, src_node, dst_node, rel),
    CONSTRAINT memory_edges_team_id_fkey
        FOREIGN KEY (team_id) REFERENCES public.teams(id) ON DELETE RESTRICT,
    CONSTRAINT memory_edges_bot_id_fkey
        FOREIGN KEY (team_id, bot_id) REFERENCES public.bots(team_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_memory_edges_src ON public.memory_edges (team_id, bot_id, src_node);
CREATE INDEX IF NOT EXISTS idx_memory_edges_dst ON public.memory_edges (team_id, bot_id, dst_node);
CREATE INDEX IF NOT EXISTS idx_memory_edges_rel ON public.memory_edges (team_id, bot_id, rel);

ALTER TABLE public.memory_edges ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.memory_edges FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS memory_edges_team_select ON public.memory_edges;
CREATE POLICY memory_edges_team_select ON public.memory_edges
    FOR SELECT USING (team_id = public.memoh_current_team_id());
DROP POLICY IF EXISTS memory_edges_team_insert ON public.memory_edges;
CREATE POLICY memory_edges_team_insert ON public.memory_edges
    FOR INSERT WITH CHECK (team_id = public.memoh_current_team_id());
DROP POLICY IF EXISTS memory_edges_team_update ON public.memory_edges;
CREATE POLICY memory_edges_team_update ON public.memory_edges
    FOR UPDATE
    USING (team_id = public.memoh_current_team_id())
    WITH CHECK (team_id = public.memoh_current_team_id());
DROP POLICY IF EXISTS memory_edges_team_delete ON public.memory_edges;
CREATE POLICY memory_edges_team_delete ON public.memory_edges
    FOR DELETE USING (team_id = public.memoh_current_team_id());
