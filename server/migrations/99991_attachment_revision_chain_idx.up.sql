-- One row per (chain, revision): two racing re-uploads cannot both become v2.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS attachment_revision_chain_uniq
    ON attachment (revision_root, revision)
    WHERE revision_root IS NOT NULL;
