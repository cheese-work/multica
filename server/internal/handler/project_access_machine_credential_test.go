package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/projectaccess"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-9684 (A5e P2 1): project access is human-only. A cloud PAT carries its
// member owner's user id, so resolveActor reads it as a member; the guard must
// refuse every machine credential, not only the agent a task token resolves to.
func TestProjectAccessMutationsRejectMachineCredentials(t *testing.T) {
	for _, source := range []string{"task_token", "cloud_pat"} {
		t.Run(source, func(t *testing.T) {
			f := newPrivateProjectFixture(t)
			as := func(req *http.Request) *http.Request {
				// testUserID is the workspace owner and a project member.
				req.Header.Set("X-Actor-Source", source)
				return asUser(req, testUserID)
			}
			projectPath := "/api/projects/" + f.projectID
			// A task token's agent is no member of the private project, so it is
			// refused earlier (404); a cloud PAT carries the owner's membership
			// and must reach, and fail, the human-only guard itself (403).
			refuse := func(call func(http.ResponseWriter, *http.Request), req *http.Request) {
				t.Helper()
				resp := testutil.Call(t, call, req)
				if source == "cloud_pat" {
					resp.Want(http.StatusForbidden)
				} else if resp.Code != http.StatusForbidden && resp.Code != http.StatusNotFound {
					t.Fatalf("%s %s: got %d, want 403 or 404", req.Method, req.URL.Path, resp.Code)
				}
			}

			refuse(testHandler.AddProjectMembers, withURLParam(as(newRequest(http.MethodPost, projectPath+"/members",
				map[string]any{"user_ids": []string{f.outsider}})), "id", f.projectID))

			removeReq := withURLParams(as(newRequest(http.MethodDelete, projectPath+"/members/"+f.insider, nil)),
				"id", f.projectID, "userId", f.insider)
			refuse(testHandler.RemoveProjectMember, removeReq)

			refuse(testHandler.UpdateProject, withURLParam(as(newRequest(http.MethodPut, projectPath,
				map[string]any{"visibility": projectaccess.VisibilityWorkspace})), "id", f.projectID))

			refuse(testHandler.UpdateProject, withURLParam(as(newRequest(http.MethodPut, projectPath,
				map[string]any{"lead_type": "member", "lead_id": f.outsider})), "id", f.projectID))

			refuse(testHandler.UpdateProject, withURLParam(as(newRequest(http.MethodPut, projectPath,
				map[string]any{"member_ids": []string{f.outsider}})), "id", f.projectID))

			refuse(testHandler.CreateProject, as(newRequest(http.MethodPost, "/api/projects",
				map[string]any{"title": f.marker + " machine private", "visibility": projectaccess.VisibilityPrivate,
					"member_ids": []string{testUserID}})))

			var visibility string
			var members int
			if err := testPool.QueryRow(context.Background(),
				`SELECT p.visibility, (SELECT count(*) FROM project_member WHERE project_id = p.id)
				 FROM project p WHERE p.id = $1`, f.projectID).Scan(&visibility, &members); err != nil {
				t.Fatal(err)
			}
			if visibility != projectaccess.VisibilityPrivate || members != 2 {
				t.Fatalf("refused %s mutations changed access: visibility=%q members=%d, want private/2", source, visibility, members)
			}
			var leaked int
			if err := testPool.QueryRow(context.Background(),
				`SELECT count(*) FROM project WHERE title = $1`, f.marker+" machine private").Scan(&leaked); err != nil {
				t.Fatal(err)
			}
			if leaked != 0 {
				t.Fatalf("refused %s CreateProject still created %d project(s)", source, leaked)
			}
		})
	}
}

// A human session still manages access: the guard must not over-block.
func TestProjectAccessMutationsStillAllowHumanSession(t *testing.T) {
	f := newPrivateProjectFixture(t)
	req := withURLParam(asUser(newRequest(http.MethodPost, "/api/projects/"+f.projectID+"/members",
		map[string]any{"user_ids": []string{f.outsider}}), testUserID), "id", f.projectID)
	testutil.Call(t, testHandler.AddProjectMembers, req).Want(http.StatusOK)
}
