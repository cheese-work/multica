SET LOCAL lock_timeout = '2s';

-- issue.metadata holds primitive values only (string, number, boolean). An
-- object under a key made clients reject the whole issue (CHE-3946). The API
-- already enforced this; this constraint makes the database enforce it too.
CREATE OR REPLACE FUNCTION issue_metadata_is_primitive(m jsonb) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT jsonb_typeof(m) = 'object' AND NOT EXISTS (
        SELECT 1 FROM jsonb_each(m) e
        WHERE jsonb_typeof(e.value) NOT IN ('string', 'number', 'boolean')
    )
$$;

-- Report violating rows and stop. This migration never rewrites or deletes data.
DO $$
DECLARE
    n bigint;
    ids text;
BEGIN
    SELECT count(*), string_agg(id::text, ', ') FILTER (WHERE rn <= 10)
      INTO n, ids
      FROM (SELECT id, row_number() OVER () AS rn FROM issue WHERE NOT issue_metadata_is_primitive(metadata)) v;
    IF n > 0 THEN
        RAISE EXCEPTION 'issue.metadata has % row(s) with non-primitive values, e.g. %. Fix them, then re-run.', n, ids;
    END IF;
END $$;

ALTER TABLE issue ADD CONSTRAINT issue_metadata_primitive_values
    CHECK (issue_metadata_is_primitive(metadata));
