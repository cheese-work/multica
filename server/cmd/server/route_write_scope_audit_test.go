package main

import (
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
)

// A5b (CHE-2216 plan 3.2; T16 Multica part): every non-read route of the real
// router is audited against the task-token write scope. A new route family, or
// a route added to an audited family, fails here until someone decides whether
// it changes an issue. Behavior of the issue-touching routes is tested in
// internal/handler (TestTaskTokenWriteScope*).

type writeFamily struct {
	writes int
	why    string
}

// scopedWriteRoutes are every write of the families that can change an issue
// without naming it in an /api/issues path; the middleware resolves each one to
// its issue (task_token_write_scope_indirect.go). The four human-only,
// per-user or task-derived ones (comment sub-issues, feed ack,
// retry-source-context, decision ask) resolve to no issue and are pinned so a
// change is reviewed; decision ask names its issue only through the running
// task, never a body field.
var scopedWriteRoutes = []string{
	"DELETE /api/attachments/{id}",
	"DELETE /api/comments/{commentId}/",
	"DELETE /api/comments/{commentId}/keep-replies",
	"DELETE /api/comments/{commentId}/reactions",
	"DELETE /api/comments/{commentId}/resolve",
	"DELETE /api/plugin-bridge/v1/storage/{scope}/{key}",
	"PATCH /api/plugin-bridge/v1/issues/{issue_ref}",
	"POST /api/comments/{commentId}/reactions",
	"POST /api/comments/{commentId}/resolve",
	"POST /api/comments/{commentId}/sub-issues",
	"POST /api/decisions/feed/{id}/ack",
	"POST /api/tasks/{taskId}/retry-source-context",
	"POST /api/decisions/",
	"POST /api/decisions/ask",
	"POST /api/decisions/{id}/withdraw",
	"POST /api/plugin-bridge/v1/hooks/{key}",
	"POST /api/plugin-bridge/v1/issues/{issue_ref}/comments",
	"POST /api/tasks/{taskId}/cancel",
	"POST /api/tasks/{taskId}/rerun",
	"POST /api/upload-file",
	"PUT /api/comments/{commentId}/",
	"PUT /api/plugin-bridge/v1/storage/{scope}/{key}",
}

// deniedRunTokenRoutes change issues or tasks of other runs without naming
// them (agent cancel/archive cascade to tasks, a project workflow remaps issue
// statuses, pausing a project cancels its tasks, deleting a project, label,
// squad, runtime, chat session, workspace or member detaches, reassigns or
// cancels, clearing a chat's queue cancels its tasks, prioritizing one reorders them and steering one cancels it; approving a GitHub revision projects comments onto linked issues). A run token answers 403 on each
// (DenyTaskTokenWrites); TestTaskTokenDeniedOnCrossIssueRoutes drives them.
var deniedRunTokenRoutes = []string{
	"DELETE /api/chat/sessions/{sessionId}/",
	"DELETE /api/chat/sessions/{sessionId}/queued-tasks",
	"DELETE /api/projects/{id}/",
	"POST /api/chat/sessions/{sessionId}/queued-tasks/{taskId}/prioritize",
	"POST /api/chat/sessions/{sessionId}/tasks/{taskId}/steer",
	"POST /api/github/intake/revisions/{id}/approve",
	"DELETE /api/labels/{id}/",
	"DELETE /api/runtimes/{runtimeId}/",
	"DELETE /api/squads/{id}/",
	"DELETE /api/workspaces/{id}/",
	"DELETE /api/workspaces/{id}/members/{memberId}/",
	"PATCH /api/chat/sessions/{sessionId}/archive",
	"POST /api/agents/{id}/archive",
	"POST /api/agents/{id}/cancel-tasks",
	"POST /api/projects/{id}/workflow",
	"PUT /api/projects/{id}/",
	"POST /api/runtimes/{runtimeId}/archive-agents-and-delete",
	"POST /api/runtimes/{runtimeId}/revoke-workspace-access",
	"POST /api/runtimes/{runtimeId}/unbind-agents-and-delete",
	"POST /api/workspaces/{id}/leave",
}

const (
	byHuman     = "human-only route (RequireHumanActor) or an account/billing/auth surface a run token never holds"
	byDaemon    = "daemon-token routes (DaemonAuth), never a run token"
	byPublic    = "public route with its own credential (signature, token, login)"
	byWorkspace = "workspace or agent configuration; the writes left here change no issue or task of another run"
	byOwnRow    = "bound to its own row (chat session, inbox item, pin, view, wakeup), not to an issue"
)

// otherWriteFamilies need no issue check. writes pins the route count so a new
// route in the family is reviewed.
var otherWriteFamilies = map[string]writeFamily{
	"api/agent-builder":            {3, byWorkspace},
	"api/agent-decisions":          {2, byHuman},
	"api/agents":                   {22, byWorkspace},
	"api/auth":                     {2, byPublic},
	"api/autopilots":               {12, byWorkspace},
	"api/chat":                     {25, byOwnRow},
	"api/cli-token":                {1, byHuman},
	"api/client-usage":             {1, byHuman},
	"api/cloud-billing":            {2, byHuman},
	"api/cloud-runtime":            {7, byWorkspace},
	"api/cloud-subscriptions":      {5, byHuman},
	"api/contact-sales":            {1, byPublic},
	"api/daemon":                   {35, byDaemon},
	"api/decision-actions":         {4, byHuman + "; the one run-token route, POST /proposals, requires a running task on its issue_id (ProposeDecisionAction)"},
	"api/decision-answers":         {1, byHuman},
	"api/decision-requests":        {3, byHuman},
	"api/dingtalk":                 {1, byPublic},
	"api/feedback":                 {1, byOwnRow},
	"api/github":                   {1, byWorkspace},
	"api/inbox":                    {8, byOwnRow},
	"api/integrations":             {2, byWorkspace},
	"api/invitations":              {2, byOwnRow},
	"api/issue-statuses":           {4, byWorkspace},
	"api/issue-view-preferences":   {1, byOwnRow},
	"api/issue-views":              {3, byOwnRow},
	"api/issue-workflows":          {8, byWorkspace},
	"api/labels":                   {2, byWorkspace},
	"api/lark":                     {1, byPublic},
	"api/me":                       {6, byOwnRow},
	"api/notification-channels":    {1, byOwnRow},
	"api/notification-preferences": {2, byOwnRow},
	"api/passkeys":                 {6, byHuman},
	"api/pins":                     {3, byOwnRow},
	"api/projects":                 {10, byWorkspace},
	"api/properties":               {2, byWorkspace},
	"api/provenance":               {1, byHuman},
	"api/push":                     {2, byHuman},
	"api/quick-actions":            {3, byWorkspace},
	"api/runtimes":                 {7, byWorkspace},
	"api/search-index":             {1, byHuman},
	"api/share-links":              {1, byOwnRow},
	"api/skills":                   {9, byWorkspace},
	"api/slack":                    {1, byPublic},
	"api/squads":                   {5, byWorkspace},
	"api/system-wakeups":           {1, byWorkspace},
	"api/telegram":                 {1, byPublic},
	"api/tokens":                   {3, byHuman},
	"api/wakeup-definitions":       {4, byWorkspace},
	"api/webhooks":                 {5, byPublic},
	"api/wecom":                    {1, byPublic},
	"api/workspaces":               {59, byWorkspace},
	"auth/google":                  {1, byPublic},
	"auth/logout":                  {1, byPublic},
	"auth/send-code":               {1, byPublic},
	"auth/verify-code":             {1, byPublic},
	"v1/issues":                    {2, "Public Plugin API: accepts only mpi_/mpc_ plugin tokens (PluginBearerOnly)"},
	"v1/storage":                   {2, "Public Plugin API: accepts only mpi_/mpc_ plugin tokens (PluginBearerOnly)"},
}

func TestRouterWriteRoutesAuditedAgainstTaskWriteScope(t *testing.T) {
	router := NewRouter(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil)
	families := map[string][]string{}
	var scoped, denied []string
	_ = chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
			return nil
		}
		entry := method + " " + route
		segs := strings.Split(strings.Trim(route, "/"), "/")
		family := strings.Join(segs[:min(2, len(segs))], "/")
		switch {
		case slices.Contains(deniedRunTokenRoutes, entry):
			denied = append(denied, entry)
		case family == "api/issues": // path-resolved by TaskTokenWriteScope itself
		case isScopedFamily(family):
			scoped = append(scoped, entry)
		default:
			families[family] = append(families[family], entry)
		}
		return nil
	})

	if want := sorted(deniedRunTokenRoutes); !slices.Equal(sorted(denied), want) {
		t.Errorf("denied run-token routes changed; they must exist in the router and stay behind DenyTaskTokenWrites\n got: %q\nwant: %q", sorted(denied), want)
	}
	sort.Strings(scoped)
	if want := sorted(scopedWriteRoutes); !slices.Equal(scoped, want) {
		t.Errorf("issue-touching write routes changed; audit them in task_token_write_scope_indirect.go and update scopedWriteRoutes\n got: %q\nwant: %q", scoped, want)
	}
	for family, routes := range families {
		known, ok := otherWriteFamilies[family]
		switch {
		case !ok:
			t.Errorf("unaudited write family %s: %q\nDoes it change an issue? If yes, resolve it in task_token_write_scope_indirect.go and add it to scopedWriteRoutes; if no, add it to otherWriteFamilies with the reason.", family, routes)
		case len(routes) != known.writes:
			t.Errorf("write family %s has %d routes, audited %d (%s): %q", family, len(routes), known.writes, known.why, routes)
		}
	}
	for family := range otherWriteFamilies {
		if _, ok := families[family]; !ok {
			t.Errorf("audited family %s has no write routes any more; remove it from otherWriteFamilies", family)
		}
	}
}

// isScopedFamily is true for the families whose every write route is issue-touching.
func isScopedFamily(family string) bool {
	switch family {
	case "api/comments", "api/attachments", "api/tasks", "api/decisions", "api/upload-file", "api/plugin-bridge":
		return true
	}
	return false
}

func sorted(in []string) []string {
	out := slices.Clone(in)
	sort.Strings(out)
	return out
}
