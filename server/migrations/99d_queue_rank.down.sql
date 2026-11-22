DROP FUNCTION IF EXISTS queue_rerank_slots(queue_rank_cand[]);
DROP TYPE IF EXISTS queue_rank_cand;
DROP FUNCTION IF EXISTS queue_rank(uuid, uuid, integer, timestamptz);
DROP FUNCTION IF EXISTS queue_rank_at(uuid, uuid, integer, timestamptz, timestamptz);
DROP FUNCTION IF EXISTS queue_rank_band(text);
