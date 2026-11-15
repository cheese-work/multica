package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// baseRootHeader carries the new synthetic root with a base-refresh bundle.
const baseRootHeader = "X-Multica-Base-Root"

// OwnMessage reports whether msg, the current bytes of a commit message, hash
// to the receipt of a commit this broker pushed under sha. The sha may be the
// remote's commit or the one the run knew it by. It is the one broker
// exception to the commit-message stub (plan 3.4, test T29).
func (b *Broker) OwnMessage(sha string, msg []byte) bool {
	sum := sha256.Sum256(msg)
	want := hex.EncodeToString(sum[:])
	b.rmu.Lock()
	defer b.rmu.Unlock()
	for _, rc := range b.receipts {
		for _, c := range rc.Commits {
			if (c.SHA == sha || c.RunSHA == sha) && c.MessageSHA256 == want {
				return true
			}
		}
	}
	return false
}

// handleRefresh answers a base refresh with a bundle of one tree-only root.
// The broker moves its pin to that root only after the bundle is written, so
// the next push lists it as the prerequisite. The reply holds no upstream
// commit and no upstream message (plan 3.5).
func (b *Broker) handleRefresh(w http.ResponseWriter, r *http.Request) {
	began := time.Now()
	root, status, err := b.refreshBase(r.Context(), w)
	b.log.Info("broker base refresh", "task", b.pin.Task, "repo", b.pin.Repo, "root", root, "status", status, "elapsed", time.Since(began), "error", err)
	if err != nil && status != 0 {
		http.Error(w, err.Error(), status)
		return
	}
}

// refreshBase builds the new root and, on success, streams it to w and moves
// the pin. It returns the HTTP status to answer with when it wrote nothing;
// status 0 with an error means the reply broke off after it began.
func (b *Broker) refreshBase(ctx context.Context, w http.ResponseWriter) (root string, status int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pin.Synthetic == nil {
		return "", http.StatusBadRequest, errors.New("this run has no synthetic base to refresh")
	}
	b.fmu.Lock()
	fenced := b.fenced
	b.fmu.Unlock()
	if fenced {
		return "", http.StatusForbidden, errors.New("run is fenced: base refresh is refused")
	}
	// A refresh reads the default branch tree, so it is an authorized operation
	// like a push: the server is asked now, not trusted to have sent a fence.
	// A refresh writes nothing to the remote, so any failure provably applied
	// nothing.
	op, status, err := b.authorize(ctx, inputHash("refresh", b.pin.Repo, b.pin.Ref, b.pin.Synthetic.Root))
	if err != nil {
		return "", status, err
	}
	defer func() {
		if err != nil {
			err = notApplied{err}
		}
		b.finish(op, err)
	}()
	var auth []string
	if b.pin.Tokens != nil {
		g, status, err := b.mint(ctx, false)
		if err != nil {
			return "", status, err
		}
		defer b.revoke(g)
		auth = basicAuthEnv(g.token)
	}
	ctx, cancel := context.WithTimeout(ctx, b.pin.deadline())
	defer cancel()
	defRef, err := b.defaultBranch(ctx, auth)
	if err != nil {
		return "", http.StatusServiceUnavailable, err
	}
	scratch, err := os.MkdirTemp("", "multica-broker-")
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	defer os.RemoveAll(scratch)
	work, status, err := b.baseRepoDir(ctx)
	if err != nil {
		return "", status, err
	}
	const def = "refs/multica/default"
	if out, err := b.git(ctx, work, auth, "-c", "transfer.fsckObjects=true", "fetch", "--no-tags", "--quiet", b.pin.RemoteURL, "+"+defRef+":"+def); err != nil {
		return "", http.StatusServiceUnavailable, fmt.Errorf("default branch fetch: %s", b.scrub(firstLine(out)))
	}
	tip, err := b.git(ctx, work, nil, "rev-parse", "--verify", def+"^{commit}")
	if err != nil {
		return "", http.StatusInternalServerError, fmt.Errorf("default branch tip: %s", firstLine(tip))
	}
	// A refresh only moves forward: a default branch that no longer holds the
	// current base was rewritten, and the run's commits have no safe new home.
	if tip != b.pin.Synthetic.Parent {
		if _, err := b.git(ctx, work, nil, "merge-base", "--is-ancestor", b.pin.Synthetic.Parent, tip); err != nil {
			return "", http.StatusConflict, errors.New("the current base is not an ancestor of the default branch tip; refresh refused")
		}
	}
	dest := filepath.Join(scratch, "root")
	sb, err := MaterializeSyntheticBase(ctx, work, tip, dest, "refresh")
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	out := filepath.Join(scratch, "root.bundle")
	if o, err := b.git(ctx, filepath.Join(dest, ".git"), nil, "bundle", "create", out, "refs/heads/refresh"); err != nil {
		return "", http.StatusInternalServerError, fmt.Errorf("bundle: %s", firstLine(o))
	}
	f, err := os.Open(out)
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	defer f.Close()
	if err := b.write(op); err != nil { // the fence began: deliver nothing, keep the pin
		return "", http.StatusForbidden, err
	}
	w.Header().Set(baseRootHeader, sb.Root)
	w.Header().Set("Content-Type", "application/octet-stream")
	if _, err := io.Copy(w, f); err != nil {
		return sb.Root, 0, err // the pin stays where it was; a retry rebuilds the same root
	}
	b.pinMu.Lock()
	b.pin.Base = sb.Root
	b.pin.Synthetic = &Synthetic{Root: sb.Root, Parent: sb.Source}
	b.pinMu.Unlock()
	return sb.Root, http.StatusOK, nil
}

// RefreshBase asks the broker for a new synthetic root holding the tree of the
// default branch tip, fetches it into the checkout as refs/multica/base and
// returns its SHA. The run then moves its own commits:
// git rebase --onto <root> <old root>.
func RefreshBase(ctx context.Context, socket, repoDir string) (string, error) {
	repoDir, err := repoRoot(repoDir)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://broker/base-refresh", nil)
	if err != nil {
		return "", err
	}
	resp, err := unixClient(socket).Do(req)
	if err != nil {
		return "", fmt.Errorf("broker unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("base refresh refused (%d): %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	root := resp.Header.Get(baseRootHeader)
	if !validOID(root) {
		return "", errors.New("base refresh: broker sent no root")
	}
	tmp, err := os.CreateTemp(filepath.Join(repoDir, ".git"), "base-refresh-*.bundle")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, resp.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("base refresh: %w", err)
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "fetch", "-q", tmp.Name(), "+refs/heads/refresh:refs/multica/base").CombinedOutput(); err != nil {
		return "", fmt.Errorf("base refresh: fetch: %s", firstLine(strings.TrimSpace(string(out))))
	}
	got, err := exec.CommandContext(ctx, "git", "-C", repoDir, "rev-parse", "--verify", "refs/multica/base").Output()
	if err != nil || strings.TrimSpace(string(got)) != root {
		return "", errors.New("base refresh: fetched root does not match the broker's")
	}
	return root, nil
}

// baseRepoDir returns the broker-owned bare repository that holds each fetched
// default-branch tip, so later pushes find the refreshed parent. It borrows the
// cache objects like a push scratch does and is removed when the broker closes.
func (b *Broker) baseRepoDir(ctx context.Context) (string, int, error) {
	b.pinMu.Lock()
	defer b.pinMu.Unlock()
	if b.baseRepo != "" {
		return b.baseRepo, 0, nil
	}
	dir, err := os.MkdirTemp("", "multica-broker-base-")
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	if out, err := b.git(ctx, "", nil, "init", "--bare", "--template=", dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", http.StatusInternalServerError, fmt.Errorf("init: %s", out)
	}
	info := filepath.Join(dir, "objects", "info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return "", http.StatusInternalServerError, err
	}
	if err := os.WriteFile(filepath.Join(info, "alternates"), []byte(b.pin.Objects+"\n"), 0o644); err != nil {
		_ = os.RemoveAll(dir)
		return "", http.StatusInternalServerError, err
	}
	b.baseRepo = dir
	return dir, 0, nil
}

// alternates lists the object directories a push scratch borrows.
func (b *Broker) alternates() []byte {
	b.pinMu.RLock()
	defer b.pinMu.RUnlock()
	out := b.pin.Objects + "\n"
	if b.baseRepo != "" {
		out += filepath.Join(b.baseRepo, "objects") + "\n"
	}
	return []byte(out)
}
