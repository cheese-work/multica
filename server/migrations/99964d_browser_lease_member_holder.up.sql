SET LOCAL lock_timeout = '2s';

-- Class H login lease (CHE-5989, W3-3b): a member holds it, not an agent task, so a class H row
-- may have no agent, task or issue. Additive and re-runnable.
ALTER TABLE browser_lease
    ALTER COLUMN agent_id DROP NOT NULL,
    ALTER COLUMN task_id DROP NOT NULL,
    ALTER COLUMN issue_id DROP NOT NULL;

-- Only class H may omit them: every other class keeps its agent, task and issue.
ALTER TABLE browser_lease DROP CONSTRAINT IF EXISTS browser_lease_holder_by_class;
ALTER TABLE browser_lease
    ADD CONSTRAINT browser_lease_holder_by_class CHECK (
        class = 'H' OR (agent_id IS NOT NULL AND task_id IS NOT NULL AND issue_id IS NOT NULL)
    );
