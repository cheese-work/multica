package execenv

import (
	"encoding/json"
	"errors"
	"path/filepath"
)

// prepareAntigravityMcpConfig materialises an agent's managed mcp_config as an
// agy plugin. agy has no per-run MCP flag or env var and reads its global
// ~/.gemini/config/mcp_config.json only (overriding HOME also drops the
// provider settings, so it is not an option); the one per-run location it
// loads is <cwd>/.agents/plugins/<name>/mcp_config.json (CHE-1821).
//
// agy walks UP from its cwd to the repo root when it discovers .agents, so the
// plugin is visible to every run started anywhere beneath the directory that
// holds it. That is only safe when the workdir belongs to this one task:
//
//   - A managed env or a per-task local worktree is private: sibling task dirs
//     are not on the walk, and a plugin from another task is never an ancestor.
//   - An in-place local_directory is the user's own tree, shared with humans and
//     unmanaged runs that would pick up the servers (and their header/env
//     secrets) from any subdirectory. No isolation exists there, so fail closed.
//
// The plugin is additive: agy still loads the global servers.
func prepareAntigravityMcpConfig(workDir, provider string, raw json.RawMessage, inPlace bool, manifest *sidecarManifest) error {
	if provider != "antigravity" || !hasManagedCursorMcpConfig(raw) {
		return nil
	}
	if inPlace {
		return errors.New("managed mcp_config for antigravity cannot be isolated in a shared local directory; use a managed workdir or worktree mode")
	}
	if workDir == "" {
		return errors.New("managed mcp_config requires a working directory")
	}
	dir := filepath.Join(workDir, ".agents", "plugins", "multica-mcp")
	if err := recordMkdirAll(dir, 0o700, manifest); err != nil {
		return err
	}
	// The error text names paths only: raw can hold header and env secrets.
	for _, file := range []struct {
		name string
		data []byte
	}{
		{"plugin.json", []byte(`{"name":"multica-mcp"}`)},
		{"mcp_config.json", raw},
	} {
		path := filepath.Join(dir, file.name)
		if err := recordWriteFile(path, file.data, 0o600, manifest); err != nil {
			if errors.Is(err, errPathPreExists) {
				return errors.New("managed mcp_config would overwrite existing " + path)
			}
			return err
		}
	}
	return nil
}
