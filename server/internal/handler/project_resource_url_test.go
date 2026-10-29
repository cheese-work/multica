package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestURLProjectResourceValidation(test *testing.T) {
	for _, link := range []string{
		"https://example.com/design",
		"https://congvc-c00.taila6fa8a.ts.net:34443/c/mRAnnjvGod",
		"https://example.com/design?version=2#notes",
	} {
		test.Run(link, func(test *testing.T) {
			raw, _ := json.Marshal(map[string]string{"url": "  " + link + "  "})
			normalized, err := validateAndNormalizeResourceRef("url", raw)
			if err != nil {
				test.Fatalf("HTTPS resource rejected: %v", err)
			}
			var ref map[string]string
			if err := json.Unmarshal(normalized, &ref); err != nil || ref["url"] != link || len(ref) != 1 {
				test.Fatalf("normalized ref = %s, error = %v", normalized, err)
			}
		})
	}
	for _, raw := range []string{
		`{}`, `null`, `[]`, `"https://example.com"`, `{"url":123}`, `{"url":""}`,
		`{"url":"http://example.com"}`, `{"url":"javascript:alert(1)"}`,
		`{"url":"file:///tmp/design"}`, `{"url":"//example.com/design"}`,
		`{"url":"https:///design"}`, `{"url":"https://"}`, `{"url":"https://exa\nmple.com"}`,
	} {
		test.Run(raw, func(test *testing.T) {
			if _, err := validateAndNormalizeResourceRef("url", json.RawMessage(raw)); err == nil {
				test.Fatalf("invalid URL resource accepted: %s", raw)
			}
		})
	}
}

func TestURLProjectResourceLifecycle(test *testing.T) {
	projectID := dbfx.Project(test, "URL resource lifecycle")
	path := "/api/projects/" + projectID + "/resources"
	link := "https://congvc-c00.taila6fa8a.ts.net:34443/c/mRAnnjvGod"
	var created ProjectResourceResponse
	testutil.Call(test, testHandler.CreateProjectResource, withURLParam(newRequest("POST", path, map[string]any{
		"resource_type": "url", "resource_ref": map[string]string{"url": link}, "label": "doop design file",
	}), "id", projectID)).Want(http.StatusCreated).JSON(&created)
	if created.ResourceType != "url" || created.Label == nil || *created.Label != "doop design file" {
		test.Fatalf("created URL resource = %#v", created)
	}
	var listed struct {
		Resources []ProjectResourceResponse `json:"resources"`
	}
	testutil.Call(test, testHandler.ListProjectResources, withURLParam(newRequest("GET", path, nil), "id", projectID)).Want(http.StatusOK).JSON(&listed)
	if len(listed.Resources) != 1 || listed.Resources[0].ID != created.ID || listed.Resources[0].Label == nil || *listed.Resources[0].Label != "doop design file" {
		test.Fatalf("listed URL resource = %#v", listed.Resources)
	}
	updatePath := path + "/" + created.ID
	updateRequest := testutil.WithURLParams(newRequest("PUT", updatePath, map[string]any{
		"resource_ref": map[string]string{"url": "https://example.com/updated"}, "label": "updated design",
	}), "id", projectID, "resourceId", created.ID)
	var updated ProjectResourceResponse
	testutil.Call(test, testHandler.UpdateProjectResource, updateRequest).Want(http.StatusOK).JSON(&updated)
	if updated.Label == nil || *updated.Label != "updated design" || !strings.Contains(string(updated.ResourceRef), "https://example.com/updated") {
		test.Fatalf("updated URL resource = %#v", updated)
	}
	invalidUpdate := testutil.WithURLParams(newRequest("PUT", updatePath, map[string]any{
		"resource_ref": map[string]string{"url": "http://example.com/insecure"},
	}), "id", projectID, "resourceId", created.ID)
	testutil.Call(test, testHandler.UpdateProjectResource, invalidUpdate).Want(http.StatusBadRequest)
}

func TestURLProjectResourceClaimSkipsCheckout(test *testing.T) {
	resources, repos := projectResourcesForClaim([]db.ProjectResource{
		{ResourceType: "url", ResourceRef: []byte(`{"url":"https://example.com/design"}`)},
		{ResourceType: "github_repo", ResourceRef: []byte(`{"url":"https://github.com/multica-ai/multica","ref":"main"}`)},
	})
	if len(resources) != 2 || resources[0].ResourceType != "url" {
		test.Fatalf("claim must retain the URL as context: %#v", resources)
	}
	if len(repos) != 1 || !strings.Contains(repos[0].URL, "github.com") || repos[0].Ref != "main" {
		test.Fatalf("only the github_repo resource may be checked out: %#v", repos)
	}
}

func TestURLProjectResourceTaskClaim(test *testing.T) {
	projectID := dbfx.Project(test, "Mixed project references")
	for position, resourceType := range []string{"url", "github_repo"} {
		link := "https://example.com/design"
		if resourceType == "github_repo" {
			link = "https://github.com/multica-ai/multica"
		}
		dbfx.Insert(test, "project_resource", testutil.Cols{
			"project_id": projectID, "workspace_id": testWorkspaceID,
			"resource_type": resourceType, "resource_ref": `{"url":"` + link + `"}`, "position": position,
		})
	}
	var sharedAgentID string
	dbfx.QueryRow(test, `SELECT id FROM agent WHERE workspace_id = $1 AND runtime_id = $2 LIMIT 1`, testWorkspaceID, testRuntimeID).Scan(&sharedAgentID)
	dbfx.Task(test, sharedAgentID, testutil.Cols{"runtime_id": testRuntimeID, "status": "running"})
	runtimeID := dbfx.Runtime(test, "URL resource claim runtime")
	agentID := dbfx.Agent(test, "URL resource claim agent", runtimeID)
	issueID := dbfx.Issue(test, "Task with URL context and a repository", testutil.Cols{"project_id": projectID, "priority": "medium"})
	dbfx.Task(test, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID})
	request := withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/claim", nil), "runtimeId", runtimeID)
	var response struct {
		Task *claimProjectFields `json:"task"`
	}
	testutil.Call(test, testHandler.ClaimTaskByRuntime, request).Want(http.StatusOK).JSON(&response)
	if response.Task == nil || len(response.Task.ProjectResources) != 2 {
		test.Fatalf("claim must retain both project resources: %#v", response.Task)
	}
	if len(response.Task.Repos) != 1 || response.Task.Repos[0].URL != "https://github.com/multica-ai/multica" {
		test.Fatalf("only the GitHub repository may become a checkout target: %#v", response.Task.Repos)
	}
}
