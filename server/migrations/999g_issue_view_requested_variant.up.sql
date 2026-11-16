-- My Issues views gain the "requested" relation (issues made on the viewer's
-- behalf, issue.requested_by_id). Re-adds both constraints from 269 with
-- 'requested' in the my list.
ALTER TABLE issue_view
    DROP CONSTRAINT issue_view_scope_variant_check,
    DROP CONSTRAINT issue_view_scope_variant_pairing;

ALTER TABLE issue_view
    ADD CONSTRAINT issue_view_scope_variant_check CHECK (
        scope_variant IN ('assigned', 'created', 'requested', 'involved', 'any', 'members', 'agents')
    ),
    ADD CONSTRAINT issue_view_scope_variant_pairing CHECK (
        (scope_type = 'my' AND scope_variant IN ('assigned', 'created', 'requested', 'involved', 'any'))
        OR (scope_type = 'workspace' AND (scope_variant IS NULL OR scope_variant IN ('members', 'agents')))
        OR (scope_type = 'project' AND (scope_variant IS NULL OR scope_variant IN ('members', 'agents')))
    );
