package main

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Scope rollup for the admin audit trail (CHE-5655, E4 of CHE-4807).
// auditPendingL5b holds in-scope routes still to wire and must end empty.
// auditPendingOutOfScope holds the routes deliberately left unaudited, one
// written reason each, and may only shrink.

// auditOutOfScopeCeiling is the number of out-of-scope routes at E4. Lower it
// when a route is wired or removed; never raise it.
const auditOutOfScopeCeiling = 65

// auditInScopeMatchers say which routes must be audited or exempt. A route
// under agents, runtimes, skills, squads, projects or autopilots configures
// what an agent is and can reach. The workspaces prefix is deny-by-default:
// only the allowlist below is out of scope.
var auditInScopeMatchers = []*regexp.Regexp{
	regexp.MustCompile(`^/api/(agents|runtimes|skills|squads|projects|autopilots)/`),
	regexp.MustCompile(`^/api/workspaces/\{id\}/`),
}

// auditWorkspaceOutOfScopeAllowlist are the /api/workspaces/{id}/ routes that
// may sit in auditPendingOutOfScope.
var auditWorkspaceOutOfScopeAllowlist = []string{"/api/workspaces/{id}/me/background-agents"}

func auditRouteInScope(route string) bool {
	path := route[strings.Index(route, " ")+1:]
	for _, allowed := range auditWorkspaceOutOfScopeAllowlist {
		if strings.HasPrefix(path, allowed) {
			return false
		}
	}
	for _, m := range auditInScopeMatchers {
		if m.MatchString(path) {
			return true
		}
	}
	return false
}

func TestInScopeAuditRoutesAreWired(t *testing.T) {
	var left []string
	for route := range auditPendingL5b {
		left = append(left, route)
	}
	sort.Strings(left)
	if len(left) > 0 {
		t.Errorf("%d in-scope routes are still pending; wire each or exempt it with a reason:\n  %s", len(left), strings.Join(left, "\n  "))
	}
}

func TestOutOfScopeAuditRoutesAreNotInScope(t *testing.T) {
	var bad []string
	for route, reason := range auditPendingOutOfScope {
		if strings.TrimSpace(reason) == "" {
			bad = append(bad, route+" (no written reason)")
		}
		if auditRouteInScope(route) {
			bad = append(bad, route+" (matches an in-scope matcher)")
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("out-of-scope entries that are not allowed:\n  %s", strings.Join(bad, "\n  "))
	}
}

func TestOutOfScopeAuditRatchet(t *testing.T) {
	if n := len(auditPendingOutOfScope); n > auditOutOfScopeCeiling {
		t.Errorf("out-of-scope audit routes = %d, ceiling %d; wire the new route instead of adding it", n, auditOutOfScopeCeiling)
	}
}

// The plugin config route stores plugin settings an agent runs with, so it
// must stay audited.
func TestPluginConfigRouteIsAudited(t *testing.T) {
	const route = "PUT /api/workspaces/{id}/plugins/{installationId}/config"
	if auditedRoutes[route] == "" {
		t.Errorf("%s is not in auditedRoutes", route)
	}
}

// The scope matcher itself: a /api/workspaces/{id}/ route is in scope unless
// allowlisted, so a new workspace route cannot hide in the out-of-scope map.
func TestAuditRouteScopeMatcher(t *testing.T) {
	for route, want := range map[string]bool{
		"PUT /api/agents/{id}/env":                               true,
		"POST /api/squads/{id}/members":                          true,
		"PUT /api/workspaces/{id}/qoder":                         true,
		"PUT /api/workspaces/{id}/me/background-agents":          false,
		"DELETE /api/workspaces/{id}/me/background-agents/{job}": false,
		"POST /api/tokens/":                                      false,
		"POST /api/webhooks/github":                              false,
	} {
		if got := auditRouteInScope(route); got != want {
			t.Errorf("auditRouteInScope(%q) = %v, want %v", route, got, want)
		}
	}
}
