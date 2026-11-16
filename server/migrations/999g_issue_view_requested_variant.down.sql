-- 'any' is the closest older relation (it already includes requested issues
-- after this change); rows must move first or the restored constraints reject them.
UPDATE issue_view SET scope_variant = 'any' WHERE scope_variant = 'requested';

ALTER TABLE issue_view
    DROP CONSTRAINT issue_view_scope_variant_check,
    DROP CONSTRAINT issue_view_scope_variant_pairing;

ALTER TABLE issue_view
    ADD CONSTRAINT issue_view_scope_variant_check CHECK (
        scope_variant IN ('assigned', 'created', 'involved', 'any', 'members', 'agents')
    ),
    ADD CONSTRAINT issue_view_scope_variant_pairing CHECK (
        (scope_type = 'my' AND scope_variant IN ('assigned', 'created', 'involved', 'any'))
        OR (scope_type = 'workspace' AND (scope_variant IS NULL OR scope_variant IN ('members', 'agents')))
        OR (scope_type = 'project' AND (scope_variant IS NULL OR scope_variant IN ('members', 'agents')))
    );
