-- CHE-6987: validate the constraints added NOT VALID by the previous migration.
ALTER TABLE task_pr_event_delivery VALIDATE CONSTRAINT task_pr_event_delivery_status_check;
ALTER TABLE task_pr_event_delivery VALIDATE CONSTRAINT task_pr_event_delivery_item_kind_check;
