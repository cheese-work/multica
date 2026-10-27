package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	daemonUpdateManifestFile = "manifest.json"
	daemonUpdateBinaryPrefix = "multica-"
	daemonUpdateNotAvailable = "daemon update is not available on this server"
)

// DaemonUpdateBinary describes one daemon build in the manifest.
type DaemonUpdateBinary struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// DaemonUpdateManifest is written next to the binaries by
// scripts/build-daemon-binaries.sh and served verbatim to daemons.
type DaemonUpdateManifest struct {
	Commit   string                        `json:"commit"`
	Version  string                        `json:"version"`
	Binaries map[string]DaemonUpdateBinary `json:"binaries"`
}

// DaemonUpdateSource serves the daemon builds baked into this server's image.
// A nil source answers 404 on every route.
type DaemonUpdateSource struct {
	dir      string
	manifest DaemonUpdateManifest
}

// LoadDaemonUpdateSource validates dir once: the manifest must name
// serverCommit, and every listed binary must match its recorded size and
// sha256. A leftover or mismatched directory is rejected so the server never
// serves a build it is not running.
func LoadDaemonUpdateSource(dir, serverCommit string) (*DaemonUpdateSource, error) {
	if serverCommit == "" || serverCommit == "unknown" {
		return nil, errors.New("server build has no commit stamp")
	}
	raw, err := os.ReadFile(filepath.Join(dir, daemonUpdateManifestFile))
	if err != nil {
		return nil, err
	}
	var m DaemonUpdateManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", daemonUpdateManifestFile, err)
	}
	if m.Commit != serverCommit {
		return nil, fmt.Errorf("manifest commit %q is not the server build commit %q", m.Commit, serverCommit)
	}
	if len(m.Binaries) == 0 {
		return nil, errors.New("manifest lists no binaries")
	}
	for key, want := range m.Binaries {
		got, err := hashFile(filepath.Join(dir, daemonUpdateBinaryPrefix+key))
		if err != nil {
			return nil, fmt.Errorf("binary %s: %w", key, err)
		}
		if got.SHA256 != want.SHA256 || got.Size != want.Size {
			return nil, fmt.Errorf("binary %s does not match the manifest", key)
		}
	}
	return &DaemonUpdateSource{dir: dir, manifest: m}, nil
}

func hashFile(path string) (DaemonUpdateBinary, error) {
	f, err := os.Open(path)
	if err != nil {
		return DaemonUpdateBinary{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return DaemonUpdateBinary{}, err
	}
	return DaemonUpdateBinary{SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

// Metadata serves GET /api/daemon/update.
func (s *DaemonUpdateSource) Metadata(w http.ResponseWriter, r *http.Request) {
	if s == nil {
		writeError(w, http.StatusNotFound, daemonUpdateNotAvailable)
		return
	}
	writeJSON(w, http.StatusOK, s.manifest)
}

// Binary serves GET /api/daemon/update/binary?os=&arch=. The platform key must
// be listed in the manifest, so request input never reaches the file path
// unvalidated.
func (s *DaemonUpdateSource) Binary(w http.ResponseWriter, r *http.Request) {
	if s == nil {
		writeError(w, http.StatusNotFound, daemonUpdateNotAvailable)
		return
	}
	key := r.URL.Query().Get("os") + "-" + r.URL.Query().Get("arch")
	entry, ok := s.manifest.Binaries[key]
	if !ok {
		writeError(w, http.StatusNotFound, "no daemon build for "+key)
		return
	}
	f, err := os.Open(filepath.Join(s.dir, daemonUpdateBinaryPrefix+key))
	if err != nil {
		writeError(w, http.StatusNotFound, daemonUpdateNotAvailable)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-SHA256", entry.SHA256)
	http.ServeContent(w, r, "", time.Time{}, f)
}
