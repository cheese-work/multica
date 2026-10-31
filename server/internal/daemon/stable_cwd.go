package daemon

import (
	"path/filepath"
	"slices"
)

// stableCwdFor returns the fixed cwd a Claude run should see, or "" when the
// feature is off for this agent. The cwd sits in Claude's system prompt, so a
// per-task path forces a prompt-cache miss on everything after it. The path sits
// under WorkspacesRoot (dot dir: GC and disk usage skip it) so the daemon-task
// marker stays an ancestor of the cwd and the CLI impersonation guard still fires.
func (c Config) stableCwdFor(provider, agentName, agentID string) string {
	if c.WorkspacesRoot == "" || provider != "claude" || agentID == "" || !slices.Contains(c.StableCwdAgents, agentName) {
		return ""
	}
	return filepath.Join(c.WorkspacesRoot, ".stable-cwd", agentID)
}

// hostPath maps a path under the task's stable cwd back to its real workdir:
// the daemon runs outside the agent's mount namespace and sees only an empty dir there.
func (t activeRepoCheckoutTask) hostPath(p string) string {
	if t.StableDir == "" {
		return p
	}
	if rel, err := filepath.Rel(t.StableDir, p); err == nil && filepath.IsLocal(rel) {
		return filepath.Join(t.WorkDir, rel)
	}
	return p
}
