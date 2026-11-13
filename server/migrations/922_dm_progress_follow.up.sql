-- DM progress cards (CHE-3369 C1): which issues a DM follows, and the lane that
-- owns delivery for one DM continuation chain.

-- One row per DM lineage. lineage_id is the root session of the continuation
-- chain, so follow state stays stable when a DM is compacted into a successor.
-- Every follow write takes this row FOR UPDATE. No FKs: chat session and issue
-- delete sweep these tables in application code.
CREATE TABLE dm_progress_lane (
    lineage_id UUID PRIMARY KEY,
    workspace_id UUID NOT NULL,
    owner_user_id UUID NOT NULL,
    last_card_at TIMESTAMPTZ,
    next_seq BIGINT NOT NULL DEFAULT 1,
    suspended_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE dm_progress_follow (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    lineage_id UUID NOT NULL,
    owner_user_id UUID NOT NULL,
    source TEXT NOT NULL CHECK (source IN ('origin', 'child', 'manual')),
    state TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'unfollowed')),
    muted_at TIMESTAMPTZ,
    last_rendered_status TEXT NOT NULL,
    last_immediate_at TIMESTAMPTZ,
    parent_follow_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
