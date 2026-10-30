// Package broker is the host side of the sandbox push path (CHE-2216 slice
// A3a). The run holds no GitHub credential. It hands a git bundle of its task
// branch to a per-run Unix socket; the broker checks the ref and repository
// against the pin it was built with, then pushes from the host.
package broker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// maxBundle caps one request body. Bundles stream to disk, never into memory.
const maxBundle = 1 << 30

// Pin is everything a run may push to. It comes from the daemon, never from
// the run.
type Pin struct {
	Repo      string `json:"repo"` // numeric repository ID from the claim
	Ref       string `json:"ref"`  // the only ref that may be pushed, refs/heads/...
	Base      string `json:"base"` // commit the task branch started from (bundle prerequisite)
	RemoteURL string `json:"-"`    // push target; the run never supplies a URL
	Objects   string `json:"-"`    // cache object directory, borrowed so bundle prerequisites resolve
}

// gitEnv keeps host git config, hooks and prompts out of broker git runs.
var gitEnv = []string{
	"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true", "LC_ALL=C",
}

// Broker serves one run's pin on one Unix socket.
type Broker struct {
	pin    Pin
	log    *slog.Logger
	ln     net.Listener
	srv    *http.Server
	socket string
	mu     sync.Mutex // one push at a time per run
}

// New starts a broker listening on socket (mode 0600).
func New(p Pin, socket string, log *slog.Logger) (*Broker, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	_ = os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("broker: %w", err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("broker: %w", err)
	}
	b := &Broker{pin: p, log: log, ln: ln, socket: socket}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /pin", b.handlePin)
	mux.HandleFunc("POST /push", b.handlePush)
	b.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go b.srv.Serve(ln)
	return b, nil
}

// Close stops the broker and removes the socket.
func (b *Broker) Close() error {
	err := b.srv.Close()
	_ = os.Remove(b.socket)
	return err
}

func (p Pin) validate() error {
	switch {
	case p.Repo == "":
		return errors.New("broker: pin has no repository")
	case p.RemoteURL == "":
		return errors.New("broker: pin has no remote")
	case !strings.HasPrefix(p.Ref, "refs/heads/") || !validRef(p.Ref):
		return fmt.Errorf("broker: pin ref %q is not a branch", p.Ref)
	case !filepath.IsAbs(p.Objects):
		return errors.New("broker: pin objects directory is not absolute")
	}
	return nil
}

func validRef(ref string) bool {
	return exec.Command("git", "check-ref-format", ref).Run() == nil
}

// handlePin tells the in-sandbox client what to bundle.
func (b *Broker) handlePin(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(b.pin)
}

func (b *Broker) handlePush(w http.ResponseWriter, r *http.Request) {
	began := time.Now()
	sha, status, err := b.push(r)
	b.log.Info("broker push", "repo", b.pin.Repo, "ref", b.pin.Ref, "sha", sha, "status", status, "elapsed", time.Since(began), "error", err)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"ref": b.pin.Ref, "sha": sha})
}

// push runs the pin checks, unpacks the bundle in a scratch repository and
// pushes the one pinned ref without force. It returns an HTTP status for a
// refusal or failure.
func (b *Broker) push(r *http.Request) (sha string, status int, err error) {
	q := r.URL.Query()
	if q.Get("ref") != b.pin.Ref {
		return "", http.StatusForbidden, errors.New("ref is not this run's task branch")
	}
	if repo := q.Get("repo"); repo != "" && repo != b.pin.Repo {
		return "", http.StatusForbidden, errors.New("repository is not this run's repository")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	scratch, err := os.MkdirTemp("", "multica-broker-")
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	defer os.RemoveAll(scratch)

	bundle := filepath.Join(scratch, "in.bundle")
	if err := saveBody(bundle, http.MaxBytesReader(nil, r.Body, maxBundle)); err != nil {
		return "", http.StatusBadRequest, err
	}
	work := filepath.Join(scratch, "work.git")
	ctx := r.Context()
	if out, err := b.git(ctx, "", "init", "--bare", "--template=", work); err != nil {
		return "", http.StatusInternalServerError, fmt.Errorf("init: %s", out)
	}
	if err := os.MkdirAll(filepath.Join(work, "objects", "info"), 0o755); err != nil {
		return "", http.StatusInternalServerError, err
	}
	if err := os.WriteFile(filepath.Join(work, "objects", "info", "alternates"), []byte(b.pin.Objects+"\n"), 0o644); err != nil {
		return "", http.StatusInternalServerError, err
	}
	heads, err := bundleHeads(bundle)
	if err != nil {
		return "", http.StatusBadRequest, err
	}
	if len(heads) != 1 || heads[0].ref != b.pin.Ref {
		return "", http.StatusBadRequest, errors.New("bundle must hold exactly the task branch")
	}
	sha = heads[0].sha
	if out, err := b.git(ctx, work, "-c", "transfer.fsckObjects=true", "bundle", "unbundle", bundle); err != nil {
		return "", http.StatusBadRequest, fmt.Errorf("unbundle: %s", firstLine(out))
	}
	if out, err := b.git(ctx, work, "push", b.pin.RemoteURL, sha+":"+b.pin.Ref); err != nil {
		return sha, http.StatusConflict, fmt.Errorf("push: %s", strings.ReplaceAll(pushReason(out), b.pin.RemoteURL, "<remote>"))
	}
	return sha, http.StatusOK, nil
}

// git runs git with the hardened environment; dir may be empty.
func (b *Broker) git(ctx context.Context, dir string, args ...string) (string, error) {
	if dir != "" {
		args = append([]string{"--git-dir", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), gitEnv...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

type head struct{ sha, ref string }

// bundleHeads reads the ref list from a bundle's text header, so no git
// process is needed to learn what the bundle claims to hold.
func bundleHeads(path string) ([]head, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 1<<20))
	if !sc.Scan() || (sc.Text() != "# v2 git bundle" && sc.Text() != "# v3 git bundle") {
		return nil, errors.New("not a git bundle")
	}
	var heads []head
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			return heads, nil
		case line[0] == '-' || line[0] == '@': // prerequisite or capability
		default:
			sha, ref, ok := strings.Cut(line, " ")
			if !ok {
				return nil, errors.New("malformed bundle header")
			}
			heads = append(heads, head{sha, ref})
		}
	}
	return nil, errors.New("malformed bundle header")
}

func saveBody(path string, body io.Reader) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// pushReason picks the line of git push output that says why it failed.
func pushReason(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "!") || strings.HasPrefix(l, "error:") || strings.HasPrefix(l, "fatal:") {
			return l
		}
	}
	return firstLine(out)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
