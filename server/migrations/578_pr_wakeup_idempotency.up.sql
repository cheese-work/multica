CREATE TABLE issue_wakeup_pr_event (
    wakeup_id uuid NOT NULL REFERENCES issue_wakeup(id) ON DELETE CASCADE,
    event_key text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (wakeup_id, event_key)
);

INSERT INTO issue_wakeup_pr_event (wakeup_id, event_key)
SELECT DISTINCT w.id, r.event_key
FROM issue_wakeup w
JOIN issue_wakeup_receipt r ON r.wakeup_id = w.id
WHERE w.system_rule IN ('pr_merged', 'pr_checks_failed')
ON CONFLICT DO NOTHING;
