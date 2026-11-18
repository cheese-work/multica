package main

// CHE-7111: PUT /api/workspaces/{id}/ci-watch/repos/{repo} (CHE-1446 era PR
// 1506, "API and CLI for CI watcher config and events") reached main without a
// guard entry and turned the rest shard red.
func init() {
	const put = "PUT /api/workspaces/{id}/ci-watch/repos/{repo}"
	auditExemptRoutes[put] = "ciwatch.ApplyConfig writes one ci_watch_config_audit row per changed field (actor, old, new) in the same transaction as the update; a failed audit insert rolls the change back"
	delete(auditPendingL5b, put)
}
