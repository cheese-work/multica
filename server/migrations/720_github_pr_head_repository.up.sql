ALTER TABLE github_pull_request
ADD COLUMN head_repo_same boolean NOT NULL DEFAULT false;
