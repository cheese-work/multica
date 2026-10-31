package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// The webhook management API (CHE-2031, spec: Management). Each test names the
// behaviour it covers.

type whEndpointView struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	RatePerMin        int    `json:"rate_per_min"`
	DisabledAt        string `json:"disabled_at"`
	PreviousExpiresAt string `json:"previous_expires_at"`
	Secret            string `json:"secret"`
}

func whAdminCall(t *testing.T, as, method, workflowID, suffix string, body any) *testutil.Response {
	t.Helper()
	path := "/api/issue-workflows/" + workflowID + "/webhooks" + suffix
	req := withURLParams(workflowRequest(method, path, body), "id", workflowID)
	if i := strings.Split(strings.TrimPrefix(suffix, "/"), "/"); len(i) == 2 {
		req = withURLParams(req, "endpointId", i[0])
	}
	if as != "" {
		req.Header.Set("X-User-ID", as)
	}
	var h http.HandlerFunc
	switch {
	case method == http.MethodGet:
		h = testHandler.ListWorkflowWebhooks
	case strings.HasSuffix(suffix, "/rotate"):
		h = testHandler.RotateWorkflowWebhook
	case strings.HasSuffix(suffix, "/disable"):
		h = testHandler.DisableWorkflowWebhook
	default:
		h = testHandler.CreateWorkflowWebhook
	}
	return testutil.Call(t, h, req)
}

func whAdminWorkflow(t *testing.T) string {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedTestCatalog(t)
	useWebhookBox(t)
	wf := createTestWorkflowSteps(t, "Admin "+workflowTestSuffix(), statusSteps("backlog", "todo", "in_progress", "in_review"))
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM workflow_webhook_endpoint WHERE workflow_id=$1`, wf.ID)
		testPool.Exec(context.Background(), `DELETE FROM activity_log WHERE details->>'workflow_id'=$1`, wf.ID)
	})
	return wf.ID
}

func whCreate(t *testing.T, workflowID, name string) whEndpointView {
	t.Helper()
	var ep whEndpointView
	whAdminCall(t, "", http.MethodPost, workflowID, "", map[string]any{"name": name}).Want(http.StatusCreated).JSON(&ep)
	return ep
}

func whActivity(t *testing.T, workflowID, action string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM activity_log WHERE action=$1 AND details->>'workflow_id'=$2`, action, workflowID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// create returns the secret once; the secret opens with the box and
// matches the whs_ shape; list never carries it.
func TestWorkflowWebhookAdminCreateShowsSecretOnce(t *testing.T) {
	wf := whAdminWorkflow(t)
	ep := whCreate(t, wf, "ci")
	if !strings.HasPrefix(ep.Secret, "whs_") || len(ep.Secret) < 40 {
		t.Fatalf("secret = %q, want whs_ + base64url of 32 bytes", ep.Secret)
	}
	if ep.RatePerMin != 60 || ep.Name != "ci" {
		t.Fatalf("defaults wrong: %+v", ep)
	}
	var sealed []byte
	if err := testPool.QueryRow(context.Background(), `SELECT secret_sealed FROM workflow_webhook_endpoint WHERE id=$1`, ep.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), ep.Secret) {
		t.Fatal("secret stored in the clear")
	}
	opened, err := testHandler.VCSSecretBox.Open(sealed)
	if err != nil || string(opened) != ep.Secret {
		t.Fatalf("sealed secret does not open to the one shown: %v", err)
	}
	resp := whAdminCall(t, "", http.MethodGet, wf, "", nil).Want(http.StatusOK)
	if strings.Contains(resp.Body.String(), "secret") || strings.Contains(resp.Body.String(), ep.Secret) {
		t.Fatalf("list leaks a secret field: %s", resp.Body.String())
	}
	var list []whEndpointView
	resp.JSON(&list)
	if len(list) != 1 || list[0].ID != ep.ID {
		t.Fatalf("list = %+v", list)
	}
	if got := whActivity(t, wf, "workflow_webhook_created"); got != 1 {
		t.Fatalf("created activity rows = %d, want 1", got)
	}
}

// bad name 400, duplicate name 409, out-of-range rate 400.
func TestWorkflowWebhookAdminValidation(t *testing.T) {
	wf := whAdminWorkflow(t)
	whCreate(t, wf, "ci")
	whAdminCall(t, "", http.MethodPost, wf, "", map[string]any{"name": "ci"}).Want(http.StatusConflict)
	whAdminCall(t, "", http.MethodPost, wf, "", map[string]any{"name": "Bad Name"}).Want(http.StatusBadRequest)
	whAdminCall(t, "", http.MethodPost, wf, "", map[string]any{"name": "slow", "rate_per_min": 0}).Want(http.StatusBadRequest)
	whAdminCall(t, "", http.MethodPost, wf, "", map[string]any{"name": "fast", "rate_per_min": 6001}).Want(http.StatusBadRequest)
	if got := whActivity(t, wf, "workflow_webhook_created"); got != 1 {
		t.Fatalf("failed creates wrote activity: %d rows", got)
	}
}

// rotate shows a new secret once, keeps the old one valid for 24 h,
// and writes one activity row. A second rotate replaces the previous secret.
func TestWorkflowWebhookAdminRotateKeepsPreviousFor24h(t *testing.T) {
	wf := whAdminWorkflow(t)
	ep := whCreate(t, wf, "ci")
	var rotated whEndpointView
	whAdminCall(t, "", http.MethodPost, wf, "/"+ep.ID+"/rotate", nil).Want(http.StatusOK).JSON(&rotated)
	if !strings.HasPrefix(rotated.Secret, "whs_") || rotated.Secret == ep.Secret {
		t.Fatalf("rotate secret = %q", rotated.Secret)
	}
	var cur, prev []byte
	var exp time.Time
	if err := testPool.QueryRow(context.Background(),
		`SELECT secret_sealed, previous_secret_sealed, previous_expires_at FROM workflow_webhook_endpoint WHERE id=$1`, ep.ID).Scan(&cur, &prev, &exp); err != nil {
		t.Fatal(err)
	}
	if got, _ := testHandler.VCSSecretBox.Open(cur); string(got) != rotated.Secret {
		t.Fatal("current secret is not the rotated one")
	}
	if got, _ := testHandler.VCSSecretBox.Open(prev); string(got) != ep.Secret {
		t.Fatal("previous secret is not the original")
	}
	if d := time.Until(exp); d < 23*time.Hour+50*time.Minute || d > 24*time.Hour+time.Minute {
		t.Fatalf("previous secret expires in %s, want about 24h", d)
	}
	if got := whActivity(t, wf, "workflow_webhook_rotated"); got != 1 {
		t.Fatalf("rotated activity rows = %d, want 1", got)
	}
	resp := whAdminCall(t, "", http.MethodGet, wf, "", nil).Want(http.StatusOK)
	if strings.Contains(resp.Body.String(), rotated.Secret) || strings.Contains(resp.Body.String(), ep.Secret) {
		t.Fatalf("list leaks a secret: %s", resp.Body.String())
	}
}

// disable stops the endpoint, drops the previous secret, writes one
// row, and a second disable or a rotate afterwards is a 404 that writes none.
func TestWorkflowWebhookAdminDisable(t *testing.T) {
	wf := whAdminWorkflow(t)
	ep := whCreate(t, wf, "ci")
	whAdminCall(t, "", http.MethodPost, wf, "/"+ep.ID+"/rotate", nil).Want(http.StatusOK)
	var off whEndpointView
	whAdminCall(t, "", http.MethodPost, wf, "/"+ep.ID+"/disable", nil).Want(http.StatusOK).JSON(&off)
	if off.DisabledAt == "" || off.Secret != "" {
		t.Fatalf("disable view = %+v", off)
	}
	var prev []byte
	if err := testPool.QueryRow(context.Background(), `SELECT previous_secret_sealed FROM workflow_webhook_endpoint WHERE id=$1`, ep.ID).Scan(&prev); err != nil || prev != nil {
		t.Fatalf("previous secret survived disable: %v %v", prev, err)
	}
	whAdminCall(t, "", http.MethodPost, wf, "/"+ep.ID+"/disable", nil).Want(http.StatusNotFound)
	whAdminCall(t, "", http.MethodPost, wf, "/"+ep.ID+"/rotate", nil).Want(http.StatusNotFound)
	if got := whActivity(t, wf, "workflow_webhook_disabled"); got != 1 {
		t.Fatalf("disabled activity rows = %d, want 1", got)
	}
}

// only the workflow owner and workspace admins manage endpoints; a
// plain member gets 403 on every verb, writes nothing, and cannot list.
func TestWorkflowWebhookAdminNeedsOwnerOrAdmin(t *testing.T) {
	wf := whAdminWorkflow(t)
	ep := whCreate(t, wf, "ci")
	plain := createPlainMember(t, "wh-admin-plain-"+workflowTestSuffix()+"@example.com")
	whAdminCall(t, plain, http.MethodPost, wf, "", map[string]any{"name": "other"}).Want(http.StatusForbidden)
	whAdminCall(t, plain, http.MethodGet, wf, "", nil).Want(http.StatusForbidden)
	whAdminCall(t, plain, http.MethodPost, wf, "/"+ep.ID+"/rotate", nil).Want(http.StatusForbidden)
	whAdminCall(t, plain, http.MethodPost, wf, "/"+ep.ID+"/disable", nil).Want(http.StatusForbidden)
	for action, want := range map[string]int{"workflow_webhook_created": 1, "workflow_webhook_rotated": 0, "workflow_webhook_disabled": 0} {
		if got := whActivity(t, wf, action); got != want {
			t.Fatalf("%s rows = %d, want %d", action, got, want)
		}
	}
}

// without a secret box the API answers 503 and stores nothing.
func TestWorkflowWebhookAdminWithoutBoxIs503(t *testing.T) {
	wf := whAdminWorkflow(t)
	ep := whCreate(t, wf, "ci")
	prev := testHandler.VCSSecretBox
	testHandler.VCSSecretBox = nil
	t.Cleanup(func() { testHandler.VCSSecretBox = prev })
	whAdminCall(t, "", http.MethodPost, wf, "", map[string]any{"name": "other"}).Want(http.StatusServiceUnavailable)
	whAdminCall(t, "", http.MethodPost, wf, "/"+ep.ID+"/rotate", nil).Want(http.StatusServiceUnavailable)
	var n int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM workflow_webhook_endpoint WHERE workflow_id=$1`, wf).Scan(&n); err != nil || n != 1 {
		t.Fatalf("endpoints = %d, %v", n, err)
	}
}

// an endpoint of another workflow is not reachable through this one.
func TestWorkflowWebhookAdminScopedToWorkflow(t *testing.T) {
	wf := whAdminWorkflow(t)
	other := whAdminWorkflow(t)
	ep := whCreate(t, other, "ci")
	whAdminCall(t, "", http.MethodPost, wf, "/"+ep.ID+"/rotate", nil).Want(http.StatusNotFound)
	whAdminCall(t, "", http.MethodPost, wf, "/"+ep.ID+"/disable", nil).Want(http.StatusNotFound)
}
