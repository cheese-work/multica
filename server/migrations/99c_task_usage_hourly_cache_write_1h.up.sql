-- Carry the 1-hour cache-write split (task_usage.cache_write_1h_tokens, 999k)
-- into the hourly rollup so the runtime and dashboard aggregates can price
-- 1-hour writes at the 1-hour rate (CHE-6532).
--
-- Only the uncosted half is stored: tokens from rows with a provider cost are
-- priced by the provider, so only the rows still estimated from the rate table
-- need their TTL split.
--
-- NULLABLE WITH NO DEFAULT, like the other uncosted_* columns (migration 213):
-- NULL means "this bucket has not been recomputed since the split existed", and
-- readers COALESCE it to 0, i.e. every written token at the 5-minute rate, which
-- is exactly how those buckets were priced before. No history is rewritten: a
-- bucket gets a real value when the rollup next touches it. A bare ADD COLUMN
-- is metadata-only.
ALTER TABLE task_usage_hourly
    ADD COLUMN IF NOT EXISTS uncosted_cache_write_1h_tokens BIGINT;

COMMENT ON COLUMN task_usage_hourly.uncosted_cache_write_1h_tokens IS
    'Of uncosted_cache_write_tokens, the tokens written with the 1-hour TTL. NULL on buckets not recomputed since this column existed; readers COALESCE to 0 (5-minute rate).';

-- Body is migration 213's, plus the new column in `recomputed`, the insert list
-- and the conflict SET list. Nothing else changes.
CREATE OR REPLACE FUNCTION rollup_task_usage_hourly_window(
    p_from TIMESTAMPTZ,
    p_to   TIMESTAMPTZ
)
RETURNS BIGINT
LANGUAGE plpgsql
AS $$
DECLARE
    v_rows BIGINT;
BEGIN
    IF p_from >= p_to THEN
        RETURN 0;
    END IF;

    WITH
    dirty_from_updates AS (
        SELECT DISTINCT
            task_usage_hour_bucket(tu.created_at) AS bucket_hour,
            a.workspace_id                        AS workspace_id,
            atq.runtime_id                        AS runtime_id,
            atq.agent_id                          AS agent_id,
            i.project_id                          AS project_id,
            tu.provider                           AS provider,
            tu.model                              AS model
          FROM task_usage tu
          JOIN agent_task_queue atq ON atq.id      = tu.task_id
          JOIN agent            a   ON a.id        = atq.agent_id
          LEFT JOIN issue       i   ON i.id        = atq.issue_id
         WHERE atq.runtime_id IS NOT NULL
           AND (
                (tu.updated_at >= p_from AND tu.updated_at < p_to)
                -- Legacy updated_at-NULL rows; partial index from 078.
                OR (tu.updated_at IS NULL
                    AND tu.created_at >= p_from
                    AND tu.created_at <  p_to)
           )
    ),
    dirty_from_queue AS (
        SELECT bucket_hour, workspace_id, runtime_id, agent_id,
               project_id, provider, model
          FROM task_usage_hourly_dirty
         WHERE enqueued_at < p_to
    ),
    dirty_keys AS (
        SELECT * FROM dirty_from_updates
        UNION
        SELECT * FROM dirty_from_queue
    ),
    recomputed AS (
        SELECT
            dk.bucket_hour,
            dk.workspace_id,
            dk.runtime_id,
            dk.agent_id,
            dk.project_id,
            dk.provider,
            dk.model,
            SUM(tu.input_tokens)::bigint       AS input_tokens,
            SUM(tu.output_tokens)::bigint      AS output_tokens,
            SUM(tu.cache_read_tokens)::bigint  AS cache_read_tokens,
            SUM(tu.cache_write_tokens)::bigint AS cache_write_tokens,
            -- Authoritative half: only rows the provider priced.
            COALESCE(SUM(tu.cost_usd_ticks), 0)::bigint AS cost_usd_ticks,
            -- Estimated half: tokens from rows the provider did not price.
            COALESCE(SUM(tu.input_tokens)       FILTER (WHERE tu.cost_usd_ticks IS NULL), 0)::bigint AS uncosted_input_tokens,
            COALESCE(SUM(tu.output_tokens)      FILTER (WHERE tu.cost_usd_ticks IS NULL), 0)::bigint AS uncosted_output_tokens,
            COALESCE(SUM(tu.cache_read_tokens)  FILTER (WHERE tu.cost_usd_ticks IS NULL), 0)::bigint AS uncosted_cache_read_tokens,
            COALESCE(SUM(tu.cache_write_tokens) FILTER (WHERE tu.cost_usd_ticks IS NULL), 0)::bigint AS uncosted_cache_write_tokens,
            -- The 1-hour-TTL part of those written tokens. Rows with no TTL
            -- split (NULL) add nothing, so their writes keep the 5-minute rate.
            COALESCE(SUM(tu.cache_write_1h_tokens) FILTER (WHERE tu.cost_usd_ticks IS NULL), 0)::bigint AS uncosted_cache_write_1h_tokens,
            COUNT(DISTINCT tu.task_id)::bigint AS task_count,
            COUNT(*)::bigint                   AS event_count
          FROM dirty_keys dk
          JOIN agent_task_queue atq ON atq.runtime_id  = dk.runtime_id
                                    AND atq.agent_id    = dk.agent_id
          JOIN agent            a   ON a.id            = atq.agent_id
                                    AND a.workspace_id = dk.workspace_id
          LEFT JOIN issue       i   ON i.id            = atq.issue_id
          JOIN task_usage       tu  ON tu.task_id      = atq.id
                                    AND tu.provider    = dk.provider
                                    AND tu.model       = dk.model
                                    AND task_usage_hour_bucket(tu.created_at) = dk.bucket_hour
         WHERE (i.project_id IS NOT DISTINCT FROM dk.project_id)
         GROUP BY 1, 2, 3, 4, 5, 6, 7
    ),
    upserted AS (
        INSERT INTO task_usage_hourly AS d (
            bucket_hour, workspace_id, runtime_id, agent_id,
            project_id, provider, model,
            input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
            cost_usd_ticks,
            uncosted_input_tokens, uncosted_output_tokens,
            uncosted_cache_read_tokens, uncosted_cache_write_tokens,
            uncosted_cache_write_1h_tokens,
            task_count, event_count
        )
        SELECT
            bucket_hour, workspace_id, runtime_id, agent_id,
            project_id, provider, model,
            input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
            cost_usd_ticks,
            uncosted_input_tokens, uncosted_output_tokens,
            uncosted_cache_read_tokens, uncosted_cache_write_tokens,
            uncosted_cache_write_1h_tokens,
            task_count, event_count
          FROM recomputed
        ON CONFLICT ON CONSTRAINT uq_task_usage_hourly_key DO UPDATE
            SET input_tokens                = EXCLUDED.input_tokens,
                output_tokens               = EXCLUDED.output_tokens,
                cache_read_tokens           = EXCLUDED.cache_read_tokens,
                cache_write_tokens          = EXCLUDED.cache_write_tokens,
                cost_usd_ticks              = EXCLUDED.cost_usd_ticks,
                uncosted_input_tokens       = EXCLUDED.uncosted_input_tokens,
                uncosted_output_tokens      = EXCLUDED.uncosted_output_tokens,
                uncosted_cache_read_tokens  = EXCLUDED.uncosted_cache_read_tokens,
                uncosted_cache_write_tokens = EXCLUDED.uncosted_cache_write_tokens,
                uncosted_cache_write_1h_tokens = EXCLUDED.uncosted_cache_write_1h_tokens,
                task_count                  = EXCLUDED.task_count,
                event_count                 = EXCLUDED.event_count,
                updated_at                  = now()
        RETURNING 1
    ),
    deleted_empty AS (
        DELETE FROM task_usage_hourly d
         USING dirty_keys dk
         WHERE d.bucket_hour  = dk.bucket_hour
           AND d.workspace_id = dk.workspace_id
           AND d.runtime_id   = dk.runtime_id
           AND d.agent_id     = dk.agent_id
           AND d.project_id IS NOT DISTINCT FROM dk.project_id
           AND d.provider     = dk.provider
           AND d.model        = dk.model
           AND NOT EXISTS (
               SELECT 1 FROM recomputed r
                WHERE r.bucket_hour  = dk.bucket_hour
                  AND r.workspace_id = dk.workspace_id
                  AND r.runtime_id   = dk.runtime_id
                  AND r.agent_id     = dk.agent_id
                  AND r.project_id IS NOT DISTINCT FROM dk.project_id
                  AND r.provider     = dk.provider
                  AND r.model        = dk.model
           )
        RETURNING 1
    )
    SELECT (SELECT COUNT(*) FROM upserted) + (SELECT COUNT(*) FROM deleted_empty)
      INTO v_rows;

    DELETE FROM task_usage_hourly_dirty WHERE enqueued_at < p_to;

    RETURN v_rows;
END;
$$;
