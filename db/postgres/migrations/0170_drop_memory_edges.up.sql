-- 0170_drop_memory_edges
-- Drop memory_edges. Every row was derived from memory_nodes (shared
-- profile_ref, topic, or capture day, and [[slug]] references in bodies), and
-- recall now derives those relations from the nodes in memory. Materializing
-- them stored one row per related pair, so a profile shared by n memories cost
-- n(n-1)/2 rows rewritten on every memory write.

DROP TABLE IF EXISTS public.memory_edges;
