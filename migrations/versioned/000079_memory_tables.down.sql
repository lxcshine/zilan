-- Roll back the three-layer memory tables (L2 session summaries + L3 facts).
DROP TABLE IF EXISTS memory_session_summaries;
DROP TABLE IF EXISTS memory_facts;
