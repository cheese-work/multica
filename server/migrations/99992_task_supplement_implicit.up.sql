-- CHE-5286: a member comment folded into a running turn without the author
-- asking to steer. Only the author's explicit steer shows a receipt the author
-- can retry from; an implicit one must replay on completion if the turn never
-- read it.
ALTER TABLE task_supplement ADD COLUMN implicit BOOLEAN NOT NULL DEFAULT false;
