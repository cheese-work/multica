-- Attachment revision chain (CHE-5295). A re-upload of a document becomes the
-- next revision of the earlier file instead of a second card. revision_root is
-- NULL on revision 1 and the id of revision 1 on every later revision; the
-- chain id is COALESCE(revision_root, id). sha256 is empty on older rows.
ALTER TABLE attachment
    ADD COLUMN IF NOT EXISTS revision      INT  NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS revision_of   UUID,
    ADD COLUMN IF NOT EXISTS revision_root UUID,
    ADD COLUMN IF NOT EXISTS sha256        TEXT NOT NULL DEFAULT '';
