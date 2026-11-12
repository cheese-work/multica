package cibroker

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Phase 1 events. For these GitHub loads the workflow file from the main commit.
var allowedEvents = []string{"push", "workflow_run", "schedule", "workflow_dispatch"}

// Always refused, even if a policy entry lists them (rule 4).
var refusedEvents = []string{"pull_request", "pull_request_target", "issue_comment", "merge_group"}

// Phase 1 permission ceiling: read only.
var permissionCeiling = []string{"contents", "actions", "checks", "statuses", "pull_requests", "issues", "metadata"}

// Entry binds one workflow of one repository (rule 3). The server derives the
// target repository from it, never from the request.
type Entry struct {
	Repository        string            `yaml:"repository"` // owner/name
	RepositoryID      string            `yaml:"repository_id"`
	RepositoryOwnerID string            `yaml:"repository_owner_id"`
	Workflow          string            `yaml:"workflow"` // file path, e.g. .github/workflows/cd-deploy.yml
	Events            []string          `yaml:"events"`
	Ref               string            `yaml:"ref"` // refs/heads/main only in phase 1
	RunnerEnvironment string            `yaml:"runner_environment"`
	Environments      []string          `yaml:"environments"` // "" allows a job with no environment
	WorkflowRunGuard  string            `yaml:"workflow_run_guard"`
	Permissions       map[string]string `yaml:"permissions"`
}

// Policy is the per-workflow policy file.
type Policy struct {
	Entries []Entry `yaml:"entries"`
}

// LoadPolicy parses and validates a policy file. Unknown fields are an error.
func LoadPolicy(data []byte) (*Policy, error) {
	var p Policy
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("cibroker policy: %w", err)
	}
	if len(p.Entries) == 0 {
		return nil, fmt.Errorf("cibroker policy: no entries")
	}
	seen := map[string]bool{}
	for i, e := range p.Entries {
		if err := e.validate(); err != nil {
			return nil, fmt.Errorf("cibroker policy entry %d: %w", i, err)
		}
		key := e.RepositoryID + "\x00" + e.Workflow
		if seen[key] {
			return nil, fmt.Errorf("cibroker policy entry %d: duplicate repository_id+workflow", i)
		}
		seen[key] = true
	}
	return &p, nil
}

func (e Entry) validate() error {
	switch {
	case !strings.Contains(e.Repository, "/") || strings.ContainsAny(e.Repository, "@ "):
		return fmt.Errorf("repository must be owner/name")
	case e.RepositoryID == "" || e.RepositoryOwnerID == "":
		return fmt.Errorf("repository_id and repository_owner_id are required")
	case e.Workflow == "" || strings.HasPrefix(e.Workflow, "/") || strings.Contains(e.Workflow, "@"):
		return fmt.Errorf("workflow must be a repo-relative file path")
	case e.Ref != MainRef:
		return fmt.Errorf("ref must be %s in phase 1", MainRef)
	case e.RunnerEnvironment == "":
		return fmt.Errorf("runner_environment is required")
	case len(e.Events) == 0:
		return fmt.Errorf("events is required")
	case len(e.Environments) == 0:
		return fmt.Errorf("environments is required (use [\"\"] for none)")
	case len(e.Permissions) == 0:
		return fmt.Errorf("permissions is required")
	}
	for _, ev := range e.Events {
		if !slices.Contains(allowedEvents, ev) {
			return fmt.Errorf("event %q is not allowed in phase 1", ev)
		}
	}
	if slices.Contains(e.Events, "workflow_run") && e.WorkflowRunGuard == "" {
		return fmt.Errorf("workflow_run needs workflow_run_guard citing the guard lines on main")
	}
	for perm, level := range e.Permissions {
		if !slices.Contains(permissionCeiling, perm) || level != "read" {
			return fmt.Errorf("permission %s: %s is above the phase 1 read-only ceiling", perm, level)
		}
	}
	return nil
}

// Match returns the entry that admits the claims, or a 403 *Refusal. The claims
// must already be verified. It never lets a pull_request, pull_request_target,
// issue_comment or merge_group token through, and refuses reusable-workflow
// calls (phase 1 has no reusable entries).
func (p *Policy) Match(c *Claims) (*Entry, error) {
	if slices.Contains(refusedEvents, c.EventName) {
		return nil, refuse(403, "event %s is never trusted", c.EventName)
	}
	if c.JobWorkflowRef != "" && c.JobWorkflowRef != c.WorkflowRef {
		return nil, refuse(403, "reusable workflow call")
	}
	for i := range p.Entries {
		e := &p.Entries[i]
		if e.RepositoryID != c.RepositoryID || c.WorkflowRef != e.Repository+"/"+e.Workflow+"@"+e.Ref {
			continue
		}
		switch {
		case e.RepositoryOwnerID != c.RepositoryOwnerID, e.Repository != c.Repository:
			return nil, refuse(403, "repository identity mismatch")
		case !slices.Contains(e.Events, c.EventName):
			return nil, refuse(403, "event %s not allowed for this workflow", c.EventName)
		case c.Ref != e.Ref:
			return nil, refuse(403, "ref %s not allowed", c.Ref)
		case c.RunnerEnvironment != e.RunnerEnvironment:
			return nil, refuse(403, "runner environment not allowed")
		case !slices.Contains(e.Environments, c.Environment):
			return nil, refuse(403, "environment not allowed")
		}
		return e, nil
	}
	return nil, refuse(403, "no policy entry for this repository and workflow")
}
