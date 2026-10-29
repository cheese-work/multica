ALTER TABLE github_webhook_revision
    ADD COLUMN id UUID NOT NULL DEFAULT gen_random_uuid();

ALTER TABLE github_webhook_dispatch
    ADD COLUMN revision_id UUID,
    ADD COLUMN state TEXT NOT NULL DEFAULT 'dispatched',
    ADD COLUMN reason TEXT NOT NULL DEFAULT 'admitted',
    ALTER COLUMN comment_id DROP NOT NULL,
    ALTER COLUMN task_id DROP NOT NULL;

UPDATE github_webhook_dispatch receipt SET revision_id = (
    SELECT revision.id FROM github_webhook_revision revision
    WHERE revision.workspace_id = receipt.workspace_id
      AND revision.object_kind = receipt.object_kind
      AND revision.object_id = receipt.object_id
      AND revision.body_hash = receipt.body_hash
);

ALTER TABLE github_webhook_dispatch
    ALTER COLUMN revision_id SET NOT NULL,
    ADD CONSTRAINT github_webhook_dispatch_effect_check CHECK (
        (state = 'denied' AND comment_id IS NULL AND task_id IS NULL) OR
        (state = 'projected' AND comment_id IS NOT NULL AND task_id IS NULL) OR
        (state = 'dispatched' AND comment_id IS NOT NULL AND task_id IS NOT NULL)
    );

ALTER TABLE comment ADD COLUMN github_notify_only BOOLEAN NOT NULL DEFAULT false;
