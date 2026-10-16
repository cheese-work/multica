package execenv

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
	if err := refuseSymlinkedComponents(workDir, dir); err != nil {
		return err
	}
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

// refuseSymlinkedComponents fails when any existing path component between
// workDir and dir is a symlink. recordMkdirAll and os.WriteFile follow links
// in ancestors, so a link tracked in the repo (.agents, .agents/plugins or the
// plugin dir) would carry the managed servers' header and env secrets out of
// the private tree to a directory other runs can read. A check-then-write race
// needs a concurrent writer inside the task's own private tree; the guard
// closes the static, repo-planted case.
func refuseSymlinkedComponents(workDir, dir string) error {
	rel, err := filepath.Rel(workDir, dir)
	if err != nil {
		return err
	}
	current := workDir
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // nothing below this point exists yet; recordMkdirAll creates real directories.
		}
		if err != nil {
			return fmt.Errorf("stat %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("managed mcp_config refused: %s is not a plain directory inside the task workdir", current)
		}
	}
	return nil
}
