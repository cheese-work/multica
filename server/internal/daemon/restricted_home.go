package daemon

import (
	"log/slog"

	"errors"
	"fmt"
	"github.com/multica-ai/multica/server/internal/daemon/sandbox"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// homeLayout is the input to the restricted-run HOME layout (CHE-2216 A2b).
type homeLayout struct {
	RootDir   string // task root; the per-run HOME lives under it, so task GC removes it
	UserHome  string // the daemon user's real home: the credential source
	Template  string // optional shared config template (settings, plugins), copied in
	Provider  string
	CodexHome string // per-task CODEX_HOME, already prepared
}

// prepareRestrictedHome builds the writable per-run HOME and puts the one
// provider credential the agent CLI needs into it as a read-only copy. Nothing
// else of the daemon user's home is copied; ~/.ssh, ~/.config/gh and the App
// key stay absent by construction. A missing credential source is not an
// error: the provider may authenticate through per-agent custom_env instead.
// Decision 1a (CHE-2216): the exfiltration risk of this copy is accepted
// until a model-call broker exists.
func prepareRestrictedHome(l homeLayout) (string, error) {
	home := filepath.Join(l.RootDir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", fmt.Errorf("restricted home: %w", err)
	}
	homeRoot, err := openVerifiedRoot(home)
	if err != nil {
		return "", fmt.Errorf("restricted home: %w", err)
	}
	defer homeRoot.Close()
	if l.Template != "" {
		if err := copyTemplate(l.Template, homeRoot); err != nil {
			return "", fmt.Errorf("restricted home template: %w", err)
		}
	}
	var src, rel string
	dstRoot := homeRoot
	switch l.Provider {
	case "claude":
		src = filepath.Join(l.UserHome, ".claude", ".credentials.json")
		rel = filepath.Join(".claude", ".credentials.json")
	case "codex":
		if l.CodexHome == "" {
			return "", errors.New("restricted home: codex run without CODEX_HOME")
		}
		src = filepath.Join(sharedCodexHome(l.UserHome), "auth.json")
		rel = "auth.json"
		if dstRoot, err = openVerifiedRoot(l.CodexHome); err != nil {
			return "", fmt.Errorf("restricted home credential: %w", err)
		}
		defer dstRoot.Close()
	default:
		return home, nil
	}
	if err := copyCredential(src, dstRoot, rel); err != nil {
		return "", fmt.Errorf("restricted home credential: %w", err)
	}
	return home, nil
}

// openVerifiedRoot opens dir as a root only if dir itself is a real directory.
// os.OpenRoot follows a symlink at its starting path, so a reused task
// directory replaced by a link would otherwise make the link target the
// trusted root. Everything below the root is then confined by os.Root.
func openVerifiedRoot(dir string) (*os.Root, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err == nil {
		var current os.FileInfo
		if current, err = os.Lstat(dir); err == nil {
			switch {
			case current.Mode()&os.ModeSymlink != 0:
				err = fmt.Errorf("%s is a symlink", dir)
			case !os.SameFile(opened, current):
				err = fmt.Errorf("%s was replaced while opening it", dir)
			}
		}
	}
	if err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}

// sharedCodexHome mirrors execenv's shared-home resolution ($CODEX_HOME, else
// ~/.codex) so the copy comes from the account the preparer symlinked.
func sharedCodexHome(userHome string) string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		if abs, err := filepath.Abs(v); err == nil {
			return abs
		}
	}
	return filepath.Join(userHome, ".codex")
}

// replaceFile removes rel (a stale file or a symlink; never followed) and
// creates it again exclusively, all inside root so no destination component
// can lead outside it.
func replaceFile(root *os.Root, rel string, mode os.FileMode) (*os.File, error) {
	if err := root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
}

// copyCredential replaces rel in root (possibly a symlink to the shared file)
// with a 0400 regular copy of src. A missing source leaves no credential at
// rel. Only the destination name is ever reported.
func copyCredential(src string, root *os.Root, rel string) error {
	in, err := os.Open(src)
	if errors.Is(err, fs.ErrNotExist) {
		if err := root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer in.Close()
	if dir := filepath.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	out, err := replaceFile(root, rel, 0o400)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// copyTemplate copies regular files and directories only: a symlink in the
// template could point at a credential, so it is skipped, never followed.
// Writes go through root, so a symlink left in a reused HOME cannot redirect them.
func copyTemplate(src string, root *os.Root) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		switch {
		case d.IsDir():
			if rel == "." {
				return nil
			}
			return root.MkdirAll(rel, 0o700)
		case d.Type().IsRegular():
			in, err := os.Open(path)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := replaceFile(root, rel, 0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				out.Close()
				return err
			}
			return out.Close()
		}
		return nil
	})
}

// dirBytes is the M2 measurement: the apparent size of everything under dir,
// like `du -sb`. Unreadable entries count as zero.
func dirBytes(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}

// applyRestrictedHome is the per-run hook. Off changes nothing (ordinary runs
// keep ambient capabilities until the A6 cutover). Audit logs the names the
// allowlist would drop. On builds the per-run HOME, points the run at it and
// returns a func that logs the HOME size at run end (M2).
func applyRestrictedHome(mode sandbox.Mode, l homeLayout, agentEnv map[string]string, log *slog.Logger) (func(), error) {
	switch mode {
	case sandbox.ModeAudit:
		_, dropped := agent.SplitEnv(os.Environ())
		log.Warn("sandbox audit: allowlist would drop inherited env names", "names", dropped)
	case sandbox.ModeOn:
		home, err := prepareRestrictedHome(l)
		if err != nil {
			return nil, err
		}
		agentEnv["HOME"] = home
		return func() { log.Info("sandbox: per-run HOME at run end", "home_bytes", dirBytes(home)) }, nil
	}
	return func() {}, nil
}
