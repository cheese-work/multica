package handler

import (
	"sort"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Audit kinds for autopilots, autopilot triggers, projects, project workflows
// and project resources (CHE-2774 L5a).
func init() {
	autopilot := auditKindSpec{
		TargetType: "autopilot",
		Allow:      []string{"title", "status", "pause_reason", "assignee_type", "assignee_id", "execution_mode", "project_id"},
		Text:       []string{"description", "issue_title_template"},
	}
	for _, kind := range []string{"autopilot.create", "autopilot.update", "autopilot.delete"} {
		auditKinds[kind] = autopilot
	}
	// webhook_token, signing_secret and event_filters are not allowlisted: the
	// two secrets must never reach the audit row, and the filters are free JSON.
	trigger := auditKindSpec{
		TargetType: "autopilot_trigger",
		Allow:      []string{"autopilot_id", "kind", "provider", "enabled", "label", "cron_expression", "timezone"},
	}
	for _, kind := range []string{"autopilot.trigger.create", "autopilot.trigger.update", "autopilot.trigger.delete"} {
		auditKinds[kind] = trigger
	}
	// decision_delegates is a list, so a change stores the marker.
	project := auditKindSpec{
		TargetType: "project",
		Allow:      []string{"title", "icon", "status", "priority", "lead_type", "lead_id", "start_date", "due_date", "workflow_id"},
		Text:       []string{"description"},
	}
	for _, kind := range []string{"project.create", "project.update", "project.delete"} {
		auditKinds[kind] = project
	}
	auditKinds["project.workflow.update"] = auditKindSpec{
		TargetType: "project",
		Allow:      []string{"workflow_id", "issues_remapped"},
	}
	// resource_ref is not allowlisted: a repo or bucket reference can carry
	// credentials in its URL.
	resource := auditKindSpec{
		TargetType: "project_resource",
		Allow:      []string{"project_id", "resource_type", "label", "position"},
	}
	for _, kind := range []string{"project.resource.create", "project.resource.update", "project.resource.delete"} {
		auditKinds[kind] = resource
	}
}

// autopilotAuditFields snapshots the autopilot columns an admin can change.
// subscribers is the sorted "type:id" set; it is not allowlisted, so a change
// stores the marker.
func autopilotAuditFields(a db.Autopilot, subscribers []string) map[string]any {
	sorted := append([]string(nil), subscribers...)
	sort.Strings(sorted)
	return map[string]any{
		"title":                a.Title,
		"description":          a.Description.String,
		"status":               a.Status,
		"pause_reason":         auditText(a.PauseReason),
		"assignee_type":        a.AssigneeType,
		"assignee_id":          uuidToString(a.AssigneeID),
		"execution_mode":       a.ExecutionMode,
		"issue_title_template": a.IssueTitleTemplate.String,
		"project_id":           optionalUUIDString(a.ProjectID),
		"subscribers":          sorted,
	}
}

// autopilotSubscriberKeys renders subscriber rows as "type:id" keys.
func autopilotSubscriberKeys(rows []db.AutopilotSubscriber) []string {
	keys := make([]string, len(rows))
	for i, row := range rows {
		keys[i] = row.UserType + ":" + uuidToString(row.UserID)
	}
	return keys
}

// autopilotTriggerAuditFields snapshots a trigger. The token and secret go in
// raw for the diff only; the kind's allowlist reduces them to the marker.
func autopilotTriggerAuditFields(t db.AutopilotTrigger) map[string]any {
	return map[string]any{
		"autopilot_id":    uuidToString(t.AutopilotID),
		"kind":            t.Kind,
		"provider":        t.Provider,
		"enabled":         t.Enabled,
		"label":           auditText(t.Label),
		"cron_expression": auditText(t.CronExpression),
		"timezone":        auditText(t.Timezone),
		"webhook_token":   t.WebhookToken.String,
		"signing_secret":  t.SigningSecret.String,
		"event_filters":   string(t.EventFilters),
	}
}

// projectAuditFields snapshots the project columns an admin can change.
func projectAuditFields(p db.Project) map[string]any {
	delegates := make([]string, len(p.DecisionDelegates))
	for i, id := range p.DecisionDelegates {
		delegates[i] = uuidToString(id)
	}
	return map[string]any{
		"title":              p.Title,
		"description":        p.Description.String,
		"icon":               auditText(p.Icon),
		"status":             p.Status,
		"priority":           p.Priority,
		"lead_type":          auditText(p.LeadType),
		"lead_id":            optionalUUIDString(p.LeadID),
		"start_date":         auditDate(p.StartDate),
		"due_date":           auditDate(p.DueDate),
		"workflow_id":        optionalUUIDString(p.WorkflowID),
		"decision_delegates": delegates,
	}
}

// projectResourceAuditFields snapshots a project resource. resource_ref goes in
// as text for the diff; the allowlist keeps its value out of the stored row.
func projectResourceAuditFields(r db.ProjectResource) map[string]any {
	return map[string]any{
		"project_id":    uuidToString(r.ProjectID),
		"resource_type": r.ResourceType,
		"resource_ref":  string(r.ResourceRef),
		"label":         auditText(r.Label),
		"position":      r.Position,
	}
}

func optionalUUIDString(u pgtype.UUID) any {
	if !u.Valid {
		return nil
	}
	return uuidToString(u)
}

func auditDate(d pgtype.Date) any {
	if !d.Valid {
		return nil
	}
	return d.Time.Format("2006-01-02")
}
