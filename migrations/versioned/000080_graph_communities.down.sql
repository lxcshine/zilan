-- Migration rollback: 000080_graph_communities
DROP INDEX IF EXISTS idx_graph_communities_kb;
DROP INDEX IF EXISTS uq_graph_communities_key;
DROP TABLE IF EXISTS graph_communities;
