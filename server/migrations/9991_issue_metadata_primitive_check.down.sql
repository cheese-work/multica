SET LOCAL lock_timeout = '2s';

ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_metadata_primitive_values;
DROP FUNCTION IF EXISTS issue_metadata_is_primitive(jsonb);
