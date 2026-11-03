-- Closed-PR replays still owed (CHE-3402). The refresh worker replays a PR that
-- GitHub reports closed while the mirror still says open. The mirror row commits
-- before the lifecycle and wakeup work, so a failure after that commit would
-- leave a merged row that nothing replays again. A row here is written before
-- the replay and removed once it finishes; a sweep retries what remains, also
-- after a restart. attempted_at rotates the sweep, so replays that keep
-- failing cannot starve newer ones. One row per PR address. No foreign keys (repo rule).
CREATE TABLE IF NOT EXISTS github_pr_closed_replay (
    installation_id bigint NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    pr_number integer NOT NULL,
    requested_at timestamptz NOT NULL DEFAULT now(),
    attempted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (installation_id, repo_owner, repo_name, pr_number)
);
