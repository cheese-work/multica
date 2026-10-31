package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SocketEnv names the variable that tells the in-sandbox client where the
// broker socket is.
const SocketEnv = "MULTICA_BROKER_SOCKET"

// Push bundles the checkout's current task branch and sends it to the broker.
func Push(ctx context.Context, socket, repoDir string, out io.Writer) error {
	return PushWithLease(ctx, socket, repoDir, "", out)
}

// TrackingLease is the --force-with-lease value that means "the remote-tracking
// ref of the task branch", as in git.
const TrackingLease = "tracking"

// Lease turns a --force-with-lease value into the remote tip the push expects:
// "" (no force), "<branch>:<sha>" for the checked-out task branch, or
// TrackingLease for refs/remotes/origin/<branch>. The broker validates the ID.
func Lease(repoDir, spec string) (string, error) {
	if spec == "" {
		return "", nil
	}
	repoDir, err := repoRoot(repoDir)
	if err != nil {
		return "", err
	}
	ref, err := headRef(repoDir)
	if err != nil {
		return "", err
	}
	branch := strings.TrimPrefix(ref, "refs/heads/")
	if spec == TrackingLease {
		out, err := exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch).Output()
		if err != nil {
			return "", fmt.Errorf("no remote-tracking ref for %s: use --force-with-lease=%s:<expected-sha>", branch, branch)
		}
		return strings.TrimSpace(string(out)), nil
	}
	name, sha, ok := strings.Cut(spec, ":")
	if !ok || sha == "" || (name != branch && name != ref) {
		return "", fmt.Errorf("--force-with-lease must name the task branch: --force-with-lease=%s:<expected-sha>", branch)
	}
	return sha, nil
}

// PushWithLease is Push that may replace the remote tip lease names.
func PushWithLease(ctx context.Context, socket, repoDir, lease string, out io.Writer) error {
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	repoDir, err := repoRoot(repoDir)
	if err != nil {
		return err
	}
	ref, err := headRef(repoDir)
	if err != nil {
		return err
	}
	var pin Pin
	if err := getJSON(ctx, hc, "http://broker/pin", &pin); err != nil {
		return err
	}
	bundle := exec.CommandContext(ctx, "git", "-C", repoDir, "bundle", "create", "-", ref, "^"+pin.Base)
	pipe, err := bundle.StdoutPipe()
	if err != nil {
		return err
	}
	if err := bundle.Start(); err != nil {
		return err
	}
	q := url.Values{"ref": {ref}, "repo": {pin.Repo}}
	if lease != "" {
		q.Set("lease", lease)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://broker/push?"+q.Encode(), pipe)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := hc.Do(req)
	if werr := bundle.Wait(); err == nil && werr != nil {
		err = fmt.Errorf("git bundle: %w", werr)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("push refused (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	_, err = fmt.Fprintf(out, "pushed %s\n", ref)
	return err
}

func getJSON(ctx context.Context, hc *http.Client, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("broker unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("broker: %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// repoRoot walks up from dir to the checkout that holds its own .git.
func repoRoot(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for d := dir; ; d = filepath.Dir(d) {
		if fi, err := os.Stat(filepath.Join(d, ".git")); err == nil && fi.IsDir() {
			return d, nil
		}
		if d == filepath.Dir(d) {
			return "", errors.New("push needs a checkout with its own .git")
		}
	}
}

// headRef reads the checked-out branch from the standalone .git/HEAD.
func headRef(repoDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(repoDir, ".git", "HEAD"))
	if err != nil {
		return "", fmt.Errorf("push needs a checkout with its own .git: %w", err)
	}
	ref, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref: ")
	if !ok {
		return "", errors.New("push needs a checked-out branch, not a detached HEAD")
	}
	return ref, nil
}
