package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A content write built from a snapshot must not restore the snapshot's project
// over a concurrent move (review round 2).
func TestBudgetBindingContentWriteKeepsCurrentProject(t *testing.T) {
	f := newRebindFixture(t, 2830920)
	ctx := context.Background()
	// The plugin request loads an issue while it is on the wide project.
	dbfx.Exec(t, `UPDATE issue SET project_id=$2 WHERE id=$1`, f.c.issueID, f.wideProj)
	stale, err := testHandler.Queries.GetIssue(ctx, parseUUID(f.c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	// A concurrent authorized request narrows the issue before body decoding ends.
	dbfx.Exec(t, `UPDATE issue SET project_id=$2 WHERE id=$1`, f.c.issueID, f.narrowProj)
	current, err := testHandler.Queries.GetIssue(ctx, stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	title := "Content patch only"
	updated, err := testHandler.IssueService.UpdateContent(ctx, stale, service.IssueContentPatch{Title: &title})
	if err != nil {
		t.Fatal(err)
	}
	before, err := issueBinding(ctx, testHandler.Queries, current.WorkspaceID, current.WorkflowID, current.ProjectID, current.MaxRunsOverride)
	if err != nil {
		t.Fatal(err)
	}
	after, err := issueBinding(ctx, testHandler.Queries, updated.WorkspaceID, updated.WorkflowID, updated.ProjectID, updated.MaxRunsOverride)
	if err != nil {
		t.Fatal(err)
	}
	var stored db.Issue
	stored, err = testHandler.Queries.GetIssue(ctx, stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("content patch changed project=%t effective cap %d -> %d budget widens=%t", stored.ProjectID != current.ProjectID, before.cap, after.cap, budgetWidens(before, after))
	if stored.ProjectID != current.ProjectID {
		t.Fatal("content-only patch restored stale project and bypassed budget guard")
	}
}

type interleavingBody struct {
	io.ReadCloser
	beforeRead func()
}

func (b *interleavingBody) Read(p []byte) (int, error) {
	if b.beforeRead != nil {
		fn := b.beforeRead
		b.beforeRead = nil
		fn()
	}
	return b.ReadCloser.Read(p)
}

func TestBudgetBindingPluginContentPatchKeepsCurrentProject(t *testing.T) {
	f := newRebindFixture(t, 2830921)
	installation := installPluginForAction(t, []string{"issues:read", "issues:write"})
	token, err := testHandler.PluginService.IssueInstallToken(context.Background(), parseUUID(installation))
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE issue SET project_id=$2 WHERE id=$1`, f.c.issueID, f.wideProj)
	req := pluginInstallTokenRequest(http.MethodPatch, "/v1/issues/"+f.c.issueID, token,
		map[string]any{"title": "Content only"}, map[string]string{"issue_ref": f.c.issueID})
	req.Body = &interleavingBody{ReadCloser: req.Body, beforeRead: func() {
		// The issue has been loaded. While the request awaits its JSON body,
		// another valid request moves it to the narrower project.
		move := withURLParam(withActorSource(newRequest(http.MethodPatch, "/api/issues/"+f.c.issueID,
			map[string]any{"project_id": f.narrowProj}), "interactive_jwt"), "id", f.c.issueID)
		res := httptest.NewRecorder()
		testHandler.UpdateIssue(res, move)
		if res.Code != http.StatusOK {
			t.Fatalf("concurrent move: %d %s", res.Code, res.Body.String())
		}
	}}
	res := httptest.NewRecorder()
	middleware.PluginBearerOnly(http.HandlerFunc(testHandler.PatchPluginIssue)).ServeHTTP(res, req)
	var project string
	dbfx.QueryRow(t, `SELECT project_id::text FROM issue WHERE id=$1`, f.c.issueID).Scan(&project)
	t.Logf("plugin PATCH status=%d restored wide project=%t", res.Code, project == f.wideProj)
	if project != f.narrowProj {
		t.Fatal("machine plugin content PATCH widened cap 2 to 50 by restoring stale project")
	}
}
