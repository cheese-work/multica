package issueworkflow

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-9075 (C1, R12): quick_create_discovery is off unless a workspace sets
// "on"; it applies only when the effective workflow has a Discovery step.

func TestQuickCreateDiscoverySetting(t *testing.T) {
	for settings, want := range map[string]bool{
		``: false, `{}`: false, `not json`: false,
		`{"quick_create_discovery":"off"}`:   false,
		`{"quick_create_discovery":"bogus"}`: false,
		`{"quick_create_discovery":"on"}`:    true,
		`{"quick_create_discovery":" On "}`:  true,
	} {
		if got := QuickCreateDiscovery([]byte(settings)); got != want {
			t.Errorf("QuickCreateDiscovery(%q) = %v, want %v", settings, got, want)
		}
	}
}

type discoveryQuerier struct {
	workflows map[pgtype.UUID]db.IssueWorkflow
	def       *db.IssueWorkflow
	project   db.Project
}

func (q discoveryQuerier) GetIssueWorkflow(_ context.Context, a db.GetIssueWorkflowParams) (db.IssueWorkflow, error) {
	if w, ok := q.workflows[a.ID]; ok {
		return w, nil
	}
	return db.IssueWorkflow{}, pgx.ErrNoRows
}

func (q discoveryQuerier) GetDefaultIssueWorkflow(context.Context, pgtype.UUID) (db.IssueWorkflow, error) {
	if q.def == nil {
		return db.IssueWorkflow{}, pgx.ErrNoRows
	}
	return *q.def, nil
}

func (q discoveryQuerier) GetProjectInWorkspace(context.Context, db.GetProjectInWorkspaceParams) (db.Project, error) {
	if !q.project.ID.Valid {
		return db.Project{}, pgx.ErrNoRows
	}
	return q.project, nil
}

func uuidN(n byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{15: n}, Valid: true} }

func workflowRow(t *testing.T, id pgtype.UUID, isDefault bool, keys ...string) db.IssueWorkflow {
	t.Helper()
	steps := make([]map[string]any, len(keys))
	for i, k := range keys {
		steps[i] = map[string]any{"status_key": k}
	}
	raw, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	return db.IssueWorkflow{ID: id, Name: "w", IsDefault: isDefault, Steps: raw}
}

func TestQuickCreateDiscoveryApplies(t *testing.T) {
	with := workflowRow(t, uuidN(1), true, "todo", "investigating", "in_progress", "done")
	without := workflowRow(t, uuidN(2), false, "todo", "in_progress", "done")
	on := []byte(`{"quick_create_discovery":"on"}`)
	proj := func(wf pgtype.UUID) db.Project {
		return db.Project{ID: uuidN(9), WorkflowID: wf}
	}
	cases := []struct {
		name     string
		settings []byte
		q        discoveryQuerier
		project  pgtype.UUID
		want     bool
	}{
		{"flag off, workflow has Discovery", []byte(`{}`), discoveryQuerier{def: &with}, pgtype.UUID{}, false},
		{"flag on, no project, Default has Discovery", on, discoveryQuerier{def: &with}, pgtype.UUID{}, true},
		{"flag on, no project, no stored Default", on, discoveryQuerier{}, pgtype.UUID{}, false},
		{"flag on, project workflow has Discovery", on, discoveryQuerier{def: &without, workflows: map[pgtype.UUID]db.IssueWorkflow{uuidN(1): with}, project: proj(uuidN(1))}, uuidN(9), true},
		{"flag on, project workflow lacks Discovery", on, discoveryQuerier{def: &with, workflows: map[pgtype.UUID]db.IssueWorkflow{uuidN(2): without}, project: proj(uuidN(2))}, uuidN(9), false},
		{"flag on, project falls back to Default with Discovery", on, discoveryQuerier{def: &with, project: proj(pgtype.UUID{})}, uuidN(9), true},
		{
			"flag on, a title-type workflow lacks Discovery",
			[]byte(`{"quick_create_discovery":"on","workflow_by_type":{"fix":"00000000-0000-0000-0000-000000000002"}}`),
			discoveryQuerier{def: &with, workflows: map[pgtype.UUID]db.IssueWorkflow{uuidN(2): without}}, pgtype.UUID{}, false,
		},
		{
			"flag on, every title-type workflow has Discovery",
			[]byte(`{"quick_create_discovery":"on","workflow_by_type":{"fix":"00000000-0000-0000-0000-000000000001"}}`),
			discoveryQuerier{def: &with, workflows: map[pgtype.UUID]db.IssueWorkflow{uuidN(1): with}}, pgtype.UUID{}, true,
		},
	}
	for _, c := range cases {
		got, err := QuickCreateDiscoveryApplies(context.Background(), c.q, c.settings, uuidN(7), c.project)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
