package broker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func (r *rig) bundleOf(t testing.TB) []byte {
	t.Helper()
	out, err := exec.Command("git", "-C", r.work, "bundle", "create", "-", r.pin.Ref, "^"+r.base).Output()
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	return out
}

func (r *rig) write(t testing.TB, path, body string) {
	t.Helper()
	full := filepath.Join(r.work, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, r.work, "add", "-A")
	git(t, r.work, "commit", "-m", "edit "+path)
}

func (r *rig) pushQuery(extra ...string) string {
	q := url.Values{"ref": {r.pin.Ref}, "repo": {r.pin.Repo}}
	for i := 0; i+1 < len(extra); i += 2 {
		q.Set(extra[i], extra[i+1])
	}
	return q.Encode()
}

// T11/T16: a change to a protected path is refused wherever it hides.
func TestPushRefusesProtectedPaths(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(*rig, testing.TB){
		"root":     func(r *rig, t testing.TB) { r.write(t, "AGENTS.md", "pwned\n") },
		"nested":   func(r *rig, t testing.TB) { r.write(t, "a/b/CLAUDE.md", "x\n") },
		"workflow": func(r *rig, t testing.TB) { r.write(t, ".github/workflows/x.yml", "x\n") },
		"skills":   func(r *rig, t testing.TB) { r.write(t, "skills/s/SKILL.md", "x\n") },
		"handoff":  func(r *rig, t testing.TB) { r.write(t, "scripts/post-handoff-comment.sh", "x\n") },
		"case":     func(r *rig, t testing.TB) { r.write(t, "docs/agents.MD", "x\n") },
		"delete": func(r *rig, t testing.TB) {
			git(t, r.work, "rm", "-q", "AGENTS.md")
			git(t, r.work, "commit", "-m", "rm")
		},
		"rename out": func(r *rig, t testing.TB) {
			git(t, r.work, "mv", ".github/ci.yml", "src/ci.yml")
			git(t, r.work, "commit", "-m", "mv")
		},
		"rename in": func(r *rig, t testing.TB) {
			_ = os.MkdirAll(filepath.Join(r.work, "skills"), 0o755)
			git(t, r.work, "mv", "src/a.go", "skills/a.go")
			git(t, r.work, "commit", "-m", "mv")
		},
		"buried": func(r *rig, t testing.TB) { r.write(t, "AGENTS.md", "x\n"); r.write(t, "src/b.go", "package b\n") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			edit(r, t)
			r.start(t)
			code, body := r.post(t, r.pushQuery(), bytes.NewReader(r.bundleOf(t)))
			if code != 403 || !strings.Contains(body, "protected path") {
				t.Fatalf("status %d (%s), want 403 protected path", code, body)
			}
			if got := r.remoteRef(t, r.pin.Ref); got != "" {
				t.Fatalf("remote gained %s at %s", r.pin.Ref, got)
			}
		})
	}
}

func TestPushAllowsOrdinaryAndLookalikePaths(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.write(t, "src/a.go", "package a // changed\n")
	r.write(t, "docs/AGENTS.md.txt", "x\n")
	r.write(t, "github/ci.yml", "x\n")
	r.write(t, "my-skills/x", "x\n")
	r.start(t)
	if code, body := r.post(t, r.pushQuery(), bytes.NewReader(r.bundleOf(t))); code != 200 {
		t.Fatalf("status %d (%s)", code, body)
	}
}

// Upstream changes to a protected path after the branch point are not the run's.
func TestPushIgnoresUpstreamProtectedChange(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.write(t, "src/a.go", "package a // changed\n")
	seed := filepath.Join(t.TempDir(), "s")
	git(t, filepath.Dir(seed), "clone", "-q", r.remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "AGENTS.md"), []byte("upstream\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "commit", "-qam", "upstream rules")
	git(t, seed, "push", "-q", "origin", "main")
	r.start(t)
	if code, body := r.post(t, r.pushQuery(), bytes.NewReader(r.bundleOf(t))); code != 200 {
		t.Fatalf("status %d (%s)", code, body)
	}
}

func TestPushFailsClosedWithoutPolicy(t *testing.T) {
	t.Parallel()
	for name, policy := range map[string]func(context.Context) ([]string, error){
		"error": func(context.Context) ([]string, error) { return nil, errors.New("server down") },
		"empty": func(context.Context) ([]string, error) { return nil, nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.write(t, "src/a.go", "package a // changed\n")
			r.pin.Policy = policy
			r.start(t)
			if code, body := r.post(t, r.pushQuery(), bytes.NewReader(r.bundleOf(t))); code != 503 {
				t.Fatalf("status %d (%s), want 503", code, body)
			}
			if got := r.remoteRef(t, r.pin.Ref); got != "" {
				t.Fatalf("remote gained %s", got)
			}
		})
	}
}

// T16: a run whose pinned ref is the remote default branch cannot push it.
func TestPushRefusesDefaultBranchPin(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.pin.Ref = "refs/heads/main"
	git(t, r.work, "checkout", "-q", "-B", "main")
	r.write(t, "src/a.go", "package a // changed\n")
	r.start(t)
	if code, body := r.post(t, "ref=refs/heads/main", bytes.NewReader(r.bundleOf(t))); code != 403 {
		t.Fatalf("status %d (%s), want 403", code, body)
	}
	if got := r.remoteRef(t, "refs/heads/main"); got != r.base {
		t.Fatalf("main moved to %s", got)
	}
}

func TestForceWithLease(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.write(t, "src/a.go", "package a // one\n")
	r.start(t)
	if code, body := r.post(t, r.pushQuery(), bytes.NewReader(r.bundleOf(t))); code != 200 {
		t.Fatalf("first push %d (%s)", code, body)
	}
	first := r.remoteRef(t, r.pin.Ref)
	git(t, r.work, "reset", "-q", "--hard", r.base)
	r.write(t, "src/a.go", "package a // two\n")
	rewrite := r.bundleOf(t)

	// A plain push of a rewrite stays refused.
	if code, _ := r.post(t, r.pushQuery(), bytes.NewReader(rewrite)); code != 409 {
		t.Fatalf("rewrite without lease: status %d, want 409", code)
	}
	// A lease on a tip the remote no longer has is refused.
	if code, _ := r.post(t, r.pushQuery("lease", strings.Repeat("a", 40)), bytes.NewReader(rewrite)); code != 409 {
		t.Fatalf("stale lease: status %d, want 409", code)
	}
	// A malformed lease never reaches git.
	for _, bad := range []string{"main", "--force", first[:10], first + "x"} {
		if code, _ := r.post(t, r.pushQuery("lease", bad), bytes.NewReader(rewrite)); code != 400 {
			t.Fatalf("lease %q: status %d, want 400", bad, code)
		}
	}
	if got := r.remoteRef(t, r.pin.Ref); got != first {
		t.Fatalf("remote moved to %s before a valid lease", got)
	}
	// The right lease rewrites the task branch, and only that.
	if code, body := r.post(t, r.pushQuery("lease", first), bytes.NewReader(rewrite)); code != 200 {
		t.Fatalf("leased rewrite %d (%s)", code, body)
	}
	if got, want := r.remoteRef(t, r.pin.Ref), git(t, r.work, "rev-parse", "HEAD"); got != want {
		t.Fatalf("remote %s, want %s", got, want)
	}
	if got := r.remoteRef(t, "refs/heads/main"); got != r.base {
		t.Fatalf("main moved to %s", got)
	}
}

// Force does not buy a way past the protected-path check.
func TestForceWithLeaseStillChecksProtectedPaths(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.write(t, "src/a.go", "package a // one\n")
	r.start(t)
	r.post(t, r.pushQuery(), bytes.NewReader(r.bundleOf(t)))
	first := r.remoteRef(t, r.pin.Ref)
	git(t, r.work, "reset", "-q", "--hard", r.base)
	r.write(t, "AGENTS.md", "pwned\n")
	if code, _ := r.post(t, r.pushQuery("lease", first), bytes.NewReader(r.bundleOf(t))); code != 403 {
		t.Fatalf("status %d, want 403", code)
	}
	if got := r.remoteRef(t, r.pin.Ref); got != first {
		t.Fatalf("remote moved to %s", got)
	}
}

func TestReceipts(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.commits(t, 2) // messages "g1 c0\n", "g1 c1\n"
	r.pin.ReceiptLog = filepath.Join(t.TempDir(), "receipts.jsonl")
	b := r.start(t)
	// A refused push leaves no receipt.
	r.write(t, "AGENTS.md", "pwned\n")
	if code, _ := r.post(t, r.pushQuery(), bytes.NewReader(r.bundleOf(t))); code != 403 {
		t.Fatalf("status %d, want 403", code)
	}
	if got := b.Receipts(); len(got) != 0 {
		t.Fatalf("receipts after a refusal: %+v", got)
	}
	git(t, r.work, "reset", "-q", "--hard", "HEAD~1")
	if code, body := r.post(t, r.pushQuery(), bytes.NewReader(r.bundleOf(t))); code != 200 {
		t.Fatalf("status %d (%s)", code, body)
	}
	got := b.Receipts()
	if len(got) != 1 {
		t.Fatalf("receipts = %+v, want 1", got)
	}
	rc := got[0]
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	want := []Commit{
		{SHA: git(t, r.work, "rev-parse", "HEAD~1"), MessageSHA256: sum("g1 c0\n")},
		{SHA: git(t, r.work, "rev-parse", "HEAD"), MessageSHA256: sum("g1 c1\n")},
	}
	if rc.Task != "task-1" || rc.Repo != "42" || rc.Ref != r.pin.Ref || rc.Forced || len(rc.Commits) != 2 || rc.Commits[0] != want[0] || rc.Commits[1] != want[1] || rc.Time.IsZero() {
		t.Fatalf("receipt = %+v, want commits %+v", rc, want)
	}
	raw, err := os.ReadFile(r.pin.ReceiptLog)
	if err != nil {
		t.Fatal(err)
	}
	var disk Receipt
	if err := json.Unmarshal(bytes.TrimSpace(raw), &disk); err != nil || disk.Task != rc.Task || len(disk.Commits) != 2 {
		t.Fatalf("receipt log %q: %v", raw, err)
	}
	// A leased rewrite is marked forced.
	git(t, r.work, "reset", "-q", "--hard", r.base)
	r.write(t, "src/a.go", "package a // two\n")
	if code, body := r.post(t, r.pushQuery("lease", rc.Commits[1].SHA), bytes.NewReader(r.bundleOf(t))); code != 200 {
		t.Fatalf("status %d (%s)", code, body)
	}
	if all := b.Receipts(); len(all) != 2 || !all[1].Forced {
		t.Fatalf("receipts = %+v, want a forced second", all)
	}
}

func TestMatchProtected(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		pat, path string
		want      bool
	}{
		{".github/**", ".github/workflows/x.yml", true},
		{".github/**", ".github", true}, // the directory root itself: a symlink or file there aliases the tree
		{".github/**", "x/.github/a", false},
		{"**/AGENTS.md", "AGENTS.md", true},
		{"**/AGENTS.md", "a/b/AGENTS.md", true},
		{"**/AGENTS.md", "a/AGENTS.md.bak", false},
		{"scripts/post-handoff-comment.sh", "scripts/post-handoff-comment.sh", true},
		{"scripts/post-handoff-comment.sh", "x/scripts/post-handoff-comment.sh", false},
		{"**/AGENTS.md", "a/agents.md", true},
		{"a/**/z", "a/z", true},
		{"a/**/z", "a/b/c/z", true},
		{"*.md", "x.md", true},
	} {
		if got := matchProtected(c.pat, c.path); got != c.want {
			t.Errorf("matchProtected(%q, %q) = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
}

func TestLeaseSpec(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.write(t, "src/a.go", "package a // one\n")
	tip := git(t, r.work, "rev-parse", "HEAD")
	short := strings.TrimPrefix(r.pin.Ref, "refs/heads/")
	for _, c := range []struct {
		name, spec, want string
		fail             bool
	}{
		{"none", "", "", false},
		{"explicit short ref", short + ":" + tip, tip, false},
		{"explicit full ref", r.pin.Ref + ":" + tip, tip, false},
		{"other branch", "main:" + tip, "", true},
		{"bare sha", tip, "", true},
		{"tracking without remote ref", "tracking", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := Lease(r.work, c.spec)
			if (err != nil) != c.fail || got != c.want {
				t.Fatalf("Lease(%q) = %q, %v; want %q, fail=%v", c.spec, got, err, c.want, c.fail)
			}
		})
	}
	git(t, r.work, "update-ref", "refs/remotes/origin/"+short, tip)
	if got, err := Lease(r.work, "tracking"); err != nil || got != tip {
		t.Fatalf("tracking = %q, %v; want %q", got, err, tip)
	}
}

func TestPushWithLeaseEndToEnd(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.write(t, "src/a.go", "package a // one\n")
	r.start(t)
	if err := Push(context.Background(), r.socket, r.work, io.Discard); err != nil {
		t.Fatal(err)
	}
	first := r.remoteRef(t, r.pin.Ref)
	git(t, r.work, "reset", "-q", "--hard", r.base)
	r.write(t, "src/a.go", "package a // two\n")
	if err := PushWithLease(context.Background(), r.socket, r.work, strings.Repeat("a", 40), io.Discard); err == nil {
		t.Fatal("stale lease pushed")
	}
	if err := PushWithLease(context.Background(), r.socket, r.work, first, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, want := r.remoteRef(t, r.pin.Ref), git(t, r.work, "rev-parse", "HEAD"); got != want {
		t.Fatalf("remote %s, want %s", got, want)
	}
}

// A huge commit message is hashed as a stream: the receipt is exact and the
// broker's own allocation stays far below the message size.
func TestReceiptHashesHugeMessageInConstantMemory(t *testing.T) {
	r := newRig(t)
	const size = 48 << 20
	msgFile := filepath.Join(t.TempDir(), "msg")
	if err := os.WriteFile(msgFile, bytes.Repeat([]byte("a long commit message line\n"), size/27), 0o644); err != nil {
		t.Fatal(err)
	}
	r.write(t, "src/a.go", "package a // big\n")
	git(t, r.work, "commit", "-q", "--amend", "--cleanup=verbatim", "-F", msgFile)
	_, msg, _ := strings.Cut(git(t, r.work, "cat-file", "commit", "HEAD"), "\n\n")
	want := sha256.Sum256([]byte(msg + "\n")) // git() trims the final newline
	b := r.start(t)
	body := bytes.NewReader(r.bundleOf(t))

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	code, out := r.post(t, r.pushQuery(), body)
	runtime.ReadMemStats(&after)
	if code != 200 {
		t.Fatalf("status %d (%s)", code, out)
	}
	if got := b.Receipts(); len(got) != 1 || len(got[0].Commits) != 1 || got[0].Commits[0].MessageSHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("receipts = %+v, want message hash %x", got, want)
	}
	if grown := after.TotalAlloc - before.TotalAlloc; grown > 16<<20 {
		t.Fatalf("allocated %d MiB while receipting a %d MiB message; want a stream", grown>>20, size>>20)
	}
}

// More commits than the cap are refused before anything is pushed.
func TestPushRefusesTooManyCommits(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.commits(t, 3)
	r.pin.MaxCommits = 2
	r.start(t)
	code, out := r.post(t, r.pushQuery(), bytes.NewReader(r.bundleOf(t)))
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d (%s), want 413", code, out)
	}
	if got := r.remoteRef(t, r.pin.Ref); got != "" {
		t.Fatalf("remote ref advanced to %s", got)
	}
}

// seedUpstream commits files to the remote default branch and merges that into
// the task branch, so they are in the branch tree but not in its diff.
func (r *rig) seedUpstream(t testing.TB, files map[string]string, links map[string]string) {
	t.Helper()
	seed := filepath.Join(t.TempDir(), "s")
	git(t, filepath.Dir(seed), "clone", "-q", r.remote, seed)
	for f, body := range files {
		full := filepath.Join(seed, f)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for l, target := range links {
		full := filepath.Join(seed, l)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.Symlink(target, full); err != nil {
			t.Fatal(err)
		}
	}
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-qm", "upstream layout")
	git(t, seed, "push", "-q", "origin", "main")
	git(t, r.work, "fetch", "-q", r.remote, "main")
	git(t, r.work, "merge", "-q", "--no-edit", "FETCH_HEAD")
}

func (r *rig) symlink(t testing.TB, link, target string) {
	t.Helper()
	full := filepath.Join(r.work, link)
	_ = os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.Symlink(target, full); err != nil {
		t.Fatal(err)
	}
	git(t, r.work, "add", "-A")
	git(t, r.work, "commit", "-qm", "link "+link)
}

// A symlink can make a protected path resolve to content at an unprotected
// one. The root of a protected directory is protected, and a change that an
// alias in the pushed tree (new or already there) exposes under a protected
// path is refused.
func TestPushRefusesDirAlias(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(*rig, testing.TB){
		"lone": func(r *rig, t testing.TB) { r.symlink(t, ".claude", "payload") },
		"new": func(r *rig, t testing.TB) {
			r.symlink(t, ".claude", "payload")
			r.write(t, "payload/skills/example/SKILL.md", "x\n")
		},
		"old": func(r *rig, t testing.TB) {
			r.seedUpstream(t, map[string]string{"payload/keep": "k\n"}, map[string]string{".claude": "payload"})
			r.write(t, "payload/skills/example/SKILL.md", "x\n")
		},
		"chain": func(r *rig, t testing.TB) {
			r.seedUpstream(t, map[string]string{"real/keep": "k\n"}, map[string]string{".claude": "p1", "p1/sub": "../real"})
			r.write(t, "real/x", "x\n")
		},
		"top": func(r *rig, t testing.TB) {
			r.pin.Policy = func(context.Context) ([]string, error) { return []string{"mirror/src/**"}, nil }
			r.seedUpstream(t, nil, map[string]string{"mirror": "."})
			r.write(t, "src/b.go", "package b\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			edit(r, t)
			r.start(t)
			code, body := r.post(t, r.pushQuery(), bytes.NewReader(r.bundleOf(t)))
			if code != 403 || !strings.Contains(body, "protected path") {
				t.Fatalf("status %d (%s), want 403 protected path", code, body)
			}
			if got := r.remoteRef(t, r.pin.Ref); got != "" {
				t.Fatalf("remote gained %s at %s", r.pin.Ref, got)
			}
		})
	}
}

// An alias into an unprotected tree is not a reason to refuse.
func TestPushAllowsAliasToUnprotectedTree(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.symlink(t, "docs/link", "../src")
	r.write(t, "src/a.go", "package a // changed\n")
	r.start(t)
	if code, body := r.post(t, r.pushQuery(), bytes.NewReader(r.bundleOf(t))); code != 200 {
		t.Fatalf("status %d (%s)", code, body)
	}
}

// One compact commit can reuse a subtree under many names. The check counts
// the paths it would expand and refuses past the cap, before any policy match
// has to scan them all.
func TestPushRefusesPathExplosion(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	leaf := mktree(t, r.work, "100644 blob "+git(t, r.work, "rev-parse", "HEAD:src/a.go")+"\tf1\n100644 blob "+git(t, r.work, "rev-parse", "HEAD:src/a.go")+"\tf2\n")
	var in strings.Builder
	in.WriteString(git(t, r.work, "ls-tree", "HEAD") + "\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&in, "040000 tree %s\tdir%03d\n", leaf, i)
	}
	root := mktree(t, r.work, in.String())
	c := git(t, r.work, "commit-tree", root, "-p", "HEAD", "-m", "explode")
	git(t, r.work, "reset", "-q", "--hard", c)
	r.pin.MaxPaths = 100
	r.start(t)
	code, out := r.post(t, r.pushQuery(), bytes.NewReader(r.bundleOf(t)))
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d (%s), want 413", code, out)
	}
	if got := r.remoteRef(t, r.pin.Ref); got != "" {
		t.Fatalf("remote gained %s at %s", r.pin.Ref, got)
	}
}

func mktree(t testing.TB, dir, entries string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "mktree")
	cmd.Stdin = strings.NewReader(entries)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("mktree: %v", err)
	}
	return strings.TrimSpace(string(out))
}
