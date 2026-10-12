-- Rollout guard (CHE-593): the claim-time checkpoint writer upserts against
-- this exact index, which is what makes "one live checkpoint per issue+agent"
-- a DB-enforced invariant instead of an application-level convention.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_issue_checkpoint_owner
    ON issue_checkpoint (issue_id, agent_id);
