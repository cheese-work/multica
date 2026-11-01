package repocache

// RepoDirName is the directory a checkout of rawURL gets inside a task workdir.
func RepoDirName(rawURL string) string { return repoNameFromURL(rawURL) }

// TaskBranchName is the branch a task's checkout carries: agent/{name}/{issue-}{task-key}.
func TaskBranchName(agentName, issueIdentifier, taskID string) string {
	return newTaskBranchName(agentName, issueIdentifier, taskID)
}
