CREATE TABLE governance_concurrency_guard (
    workspace_id UUID NOT NULL,
    resource TEXT NOT NULL CHECK (length(resource) BETWEEN 1 AND 128 AND resource = btrim(resource)),
    held_slots BIGINT NOT NULL DEFAULT 0 CHECK (held_slots >= 0)
);

CREATE TABLE governance_concurrency_hold (
    workspace_id UUID NOT NULL,
    resource TEXT NOT NULL CHECK (length(resource) BETWEEN 1 AND 128 AND resource = btrim(resource)),
    reservation_id UUID NOT NULL,
    state TEXT NOT NULL DEFAULT 'held' CHECK (state IN ('held', 'released')),
    held_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at TIMESTAMPTZ,
    CHECK ((state = 'released') = (released_at IS NOT NULL))
);
