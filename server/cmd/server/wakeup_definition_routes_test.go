package main

import (
	"io"
	"testing"
)

// The router's own feature-flag wiring is the production one: scoped wakeup
// definition writes must be closed there, at every scope, while reads work.
func TestWakeupDefinitionRoutesAreClosedByDefault(t *testing.T) {
	resp := authRequest(t, "POST", "/api/projects", map[string]any{"title": "wakeup definition routes"})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("CreateProject: %d %s", resp.StatusCode, body)
	}
	var project map[string]any
	readJSON(t, resp, &project)
	resp = authRequest(t, "POST", "/api/issues", map[string]any{"title": "wakeup definition routes", "status": "todo"})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("CreateIssue: %d %s", resp.StatusCode, body)
	}
	var issue map[string]any
	readJSON(t, resp, &issue)

	for name, base := range map[string]string{
		"workspace": "/api/wakeup-definitions",
		"project":   "/api/projects/" + project["id"].(string) + "/wakeup-definitions",
		"issue":     "/api/issues/" + issue["id"].(string) + "/wakeup-definitions",
	} {
		resp := authRequest(t, "GET", base, nil)
		var list struct {
			Definitions  []any `json:"definitions"`
			Capabilities struct {
				DefinitionWrites bool `json:"definition_writes"`
			} `json:"capabilities"`
		}
		if resp.StatusCode != 200 {
			t.Fatalf("%s list: %d", name, resp.StatusCode)
		}
		readJSON(t, resp, &list)
		if len(list.Definitions) != 0 || list.Capabilities.DefinitionWrites {
			t.Fatalf("%s list: %+v", name, list)
		}

		resp = authRequest(t, "PUT", base+"/child_done", map[string]any{"revision": 0, "config": map[string]any{"v": 1, "name": "x"}})
		var refused map[string]any
		status := resp.StatusCode
		readJSON(t, resp, &refused)
		if status != 403 || refused["code"] != "wakeup_definition_writes_closed" {
			t.Fatalf("%s write: %d %v", name, status, refused)
		}
		resp = authRequest(t, "POST", base+"/preview", map[string]any{"rule_key": "child_done", "config": map[string]any{"v": 1, "name": "x"}})
		if resp.StatusCode != 200 {
			t.Fatalf("%s preview: %d", name, resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp = authRequest(t, "POST", "/api/wakeup-definitions", map[string]any{"revision": 0, "config": map[string]any{"v": 1, "trigger": map[string]any{"kind": "pr_merged"}, "instruction": "x"}})
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("create custom rule: %d", resp.StatusCode)
	}
	resp = authRequest(t, "POST", "/api/projects/"+project["id"].(string)+"/wakeup-definitions", map[string]any{"revision": 0, "config": map[string]any{"v": 1, "trigger": map[string]any{"kind": "pr_merged"}, "instruction": "x"}})
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("create project custom rule: %d", resp.StatusCode)
	}
}
