package handler

import (
	"log/slog"
	"net/http"
)

// DenyTaskTokenWrites returns the middleware for routes that change issues or
// tasks of other runs without naming them: cancelling an agent's tasks,
// archiving an agent, switching a project's workflow, deleting a label, squad,
// runtime, chat session, workspace or member. A run's task token cannot be
// scoped to such a write, so mode "on" refuses it, "audit" logs what it would
// refuse and passes, anything else is off. Other credentials pass untouched.
func (h *Handler) DenyTaskTokenWrites(mode string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if mode != TaskWriteScopeOn && mode != TaskWriteScopeAudit {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Actor-Source") != "task_token" || r.Header.Get("X-Task-Token-Purpose") != "agent_task" || isReadMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			slog.Warn("task token write on a route denied to run tokens",
				"mode", mode, "method", r.Method, "path", r.URL.Path,
				"task_id", r.Header.Get("X-Task-ID"), "agent_id", r.Header.Get("X-Agent-ID"))
			if mode == TaskWriteScopeOn {
				writeError(w, http.StatusForbidden, "this endpoint is not available to a run's task token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
