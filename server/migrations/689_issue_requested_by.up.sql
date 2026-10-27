-- The human an agent created the issue for (CHE-2068). creator_* names who
-- made the row; requested_by_id names the member it is made on behalf of, so
-- workflow steps that need a person can resolve one.
ALTER TABLE issue ADD COLUMN requested_by_id UUID REFERENCES "user"(id) ON DELETE SET NULL;
