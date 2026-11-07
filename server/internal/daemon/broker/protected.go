package broker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Commit is one pushed commit: its ID and the SHA-256 of its raw message bytes.
type Commit struct {
	SHA           string `json:"sha"`
	RunSHA        string `json:"run_sha,omitempty"`
	MessageSHA256 string `json:"message_sha256"`
}

// Receipt records one successful push (plan 3.2 item 7). The read proxy uses
// it to return the run's own commit messages. Commit.SHA is the commit the
// remote holds; RunSHA is the SHA the run knew it by before translation.
type Receipt struct {
	Task    string    `json:"task"`
	Repo    string    `json:"repo"`
	Ref     string    `json:"ref"`
	Time    time.Time `json:"time"`
	Forced  bool      `json:"forced,omitempty"`
	Commits []Commit  `json:"commits"`
}

// Receipts returns the successful pushes of this run, oldest first.
func (b *Broker) Receipts() []Receipt {
	b.rmu.Lock()
	defer b.rmu.Unlock()
	return append([]Receipt(nil), b.receipts...)
}

func (b *Broker) record(rc Receipt) {
	b.rmu.Lock()
	b.receipts = append(b.receipts, rc)
	b.rmu.Unlock()
	if b.pin.ReceiptLog == "" {
		return
	}
	line, err := json.Marshal(rc)
	if err == nil {
		var f *os.File
		if f, err = os.OpenFile(b.pin.ReceiptLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			_, err = f.Write(append(line, '\n'))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
	}
	if err != nil {
		b.log.Error("broker receipt log", "task", b.pin.Task, "error", err)
	}
}

var oidRe = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

func validOID(s string) bool { return oidRe.MatchString(s) }

// scrub keeps the push target, which will carry a credential, out of replies.
func (b *Broker) scrub(s string) string { return strings.ReplaceAll(s, b.pin.RemoteURL, "<remote>") }

// defaultBranch reads the remote's default branch ref, so the check does not
// depend on anything the run can influence.
func (b *Broker) defaultBranch(ctx context.Context, auth []string) (string, error) {
	out, err := b.git(ctx, "", auth, "ls-remote", "--symref", b.pin.RemoteURL, "HEAD")
	if err != nil {
		return "", fmt.Errorf("default branch lookup: %s", b.scrub(firstLine(out)))
	}
	for _, l := range strings.Split(out, "\n") {
		if ref, ok := strings.CutPrefix(l, "ref: "); ok {
			if ref, _, _ = strings.Cut(ref, "\t"); strings.HasPrefix(ref, "refs/heads/") {
				return ref, nil
			}
		}
	}
	return "", errors.New("default branch lookup: remote HEAD is not a branch")
}

// checkProtected refuses a push whose tree differs from its merge base with the
// remote default branch at any path the policy protects, or at any path that a
// symlink in the pushed tree makes reachable from one. The net tree diff is what
// a merge would land; renames are split so both sides are checked. The walk is
// bounded (see treewalk.go); a push over a bound is refused with 413.
func (b *Broker) checkProtected(ctx context.Context, work, defRef, sha string, patterns []string, auth []string) (int, error) {
	const def = "refs/multica/default"
	if out, err := b.git(ctx, work, auth, "-c", "transfer.fsckObjects=true", "fetch", "--no-tags", "--quiet", b.pin.RemoteURL, "+"+defRef+":"+def); err != nil {
		return http.StatusServiceUnavailable, fmt.Errorf("default branch fetch: %s", b.scrub(firstLine(out)))
	}
	bases, err := b.git(ctx, work, nil, "merge-base", "--all", def, sha)
	if err != nil || bases == "" {
		return http.StatusForbidden, errors.New("the pushed branch shares no history with the default branch")
	}
	limit := b.pin.MaxPaths
	if limit <= 0 {
		limit = defaultMaxPaths
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	o, err := b.newObjReader(ctx, work, 2*limit)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	defer o.close()
	tipTree, err := b.git(ctx, work, nil, "rev-parse", sha+"^{tree}")
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("tip tree: %s", firstLine(tipTree))
	}
	var changed []string
	w := &walker{o: o, max: limit, sink: func(p string) error { changed = append(changed, p); return nil }}
	for _, base := range strings.Fields(bases) {
		baseTree, err := b.git(ctx, work, nil, "rev-parse", base+"^{tree}")
		if err != nil {
			return http.StatusInternalServerError, fmt.Errorf("base tree: %s", firstLine(baseTree))
		}
		if err := w.diff(baseTree, tipTree, "", 0); err != nil {
			return walkStatus(err), err
		}
	}
	links, err := w.symlinks(tipTree)
	if err != nil {
		return walkStatus(err), err
	}
	idx := indexLinks(links)
	for _, p := range changed {
		reach, err := idx.aliases(p)
		if err != nil {
			return walkStatus(err), err
		}
		for _, q := range reach {
			for _, pat := range patterns {
				if matchProtected(pat, q) {
					if q != strings.ToLower(p) {
						return http.StatusForbidden, fmt.Errorf("change to %q reaches protected path %q through a symlink (policy %q)", p, q, pat)
					}
					return http.StatusForbidden, fmt.Errorf("change to protected path %q (policy %q)", p, pat)
				}
			}
		}
	}
	return 0, nil
}

// checkTimeout bounds the whole protected-path walk of one push.
const checkTimeout = 2 * time.Minute

func walkStatus(err error) int {
	switch {
	case errors.Is(err, errBudget):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, errRefuse):
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}

// defaultMaxCommits caps the commits one push may add; maxCommitHeader caps the
// header block of one commit (everything before its message); maxCommitBytes
// caps one whole raw commit object that translate reads into memory.
const (
	defaultMaxCommits = 1000
	maxCommitHeader   = 64 << 10
	maxCommitBytes    = 1 << 20
)

var errTooManyCommits = errors.New("push adds too many commits")

// realBase is the commit the pushed history sits on: the real parent for a
// synthetic-base run, the pinned base otherwise.
func (b *Broker) realBase() string {
	if b.pin.Synthetic != nil {
		return b.pin.Synthetic.Parent
	}
	return b.pin.Base
}

// commitReceipts lists the commits the bundle added on top of the pinned base
// with the SHA-256 of each raw message. One git process serves all objects and
// its output is hashed as a stream, so a huge message costs time, not memory.
// runSHA maps a translated commit to the SHA the run knew it by (nil without
// translation).
func (b *Broker) commitReceipts(ctx context.Context, work, sha string, runSHA map[string]string) ([]Commit, error) {
	limit := b.pin.MaxCommits
	if limit <= 0 {
		limit = defaultMaxCommits
	}
	list, err := b.git(ctx, work, nil, "rev-list", "--reverse", "--max-count="+strconv.Itoa(limit+1), sha, "^"+b.realBase())
	if err != nil {
		return nil, fmt.Errorf("receipt: %s", firstLine(list))
	}
	shas := strings.Fields(list)
	if len(shas) > limit {
		return nil, errTooManyCommits
	}
	if len(shas) == 0 {
		return nil, nil
	}
	cmd := exec.CommandContext(ctx, "git", "--git-dir", work, "cat-file", "--batch")
	cmd.Env = append(os.Environ(), gitEnv...)
	cmd.Stdin = strings.NewReader(list + "\n")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("receipt: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("receipt: %w", err)
	}
	commits, readErr := readReceipts(bufio.NewReader(stdout), shas)
	if readErr != nil {
		_ = cmd.Process.Kill()
	}
	if waitErr := cmd.Wait(); readErr == nil && waitErr != nil {
		readErr = waitErr
	}
	if readErr != nil {
		return nil, fmt.Errorf("receipt: %w", readErr)
	}
	for i := range commits {
		commits[i].RunSHA = runSHA[commits[i].SHA]
	}
	return commits, nil
}

// readReceipts parses the `cat-file --batch` stream of the given commits.
func readReceipts(r *bufio.Reader, shas []string) ([]Commit, error) {
	commits := make([]Commit, 0, len(shas))
	for _, want := range shas {
		hdr, err := r.ReadString('\n')
		f := strings.Fields(hdr)
		if err != nil || len(f) != 3 || f[0] != want || f[1] != "commit" {
			return nil, fmt.Errorf("unexpected object header %q", hdr)
		}
		n, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("unexpected object size %q", f[2])
		}
		sum, err := hashMessage(r, n)
		if err != nil {
			return nil, err
		}
		if _, err := r.Discard(1); err != nil { // the newline after the object
			return nil, err
		}
		commits = append(commits, Commit{SHA: want, MessageSHA256: sum})
	}
	return commits, nil
}

// hashMessage reads one n-byte commit object and returns the SHA-256 of its
// message: the bytes after the first blank line (empty when there is none).
func hashMessage(r *bufio.Reader, n int64) (string, error) {
	var head []byte
	for {
		if i := bytes.Index(head, []byte("\n\n")); i >= 0 {
			h := sha256.New()
			h.Write(head[i+2:])
			if _, err := io.CopyN(h, r, n-int64(len(head))); err != nil {
				return "", err
			}
			return hex.EncodeToString(h.Sum(nil)), nil
		}
		if int64(len(head)) == n {
			sum := sha256.Sum256(nil)
			return hex.EncodeToString(sum[:]), nil
		}
		if len(head) >= maxCommitHeader {
			return "", errors.New("commit header too large")
		}
		chunk := min(n-int64(len(head)), 4096)
		buf := make([]byte, chunk)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		head = append(head, buf...)
	}
}

// matchProtected reports whether path falls under a policy pattern: slash
// separated segments, `*` within a segment, `**` for any number of whole
// segments (a trailing `**` may match none, so `dir/**` protects `dir` itself, which a symlink or file swap can replace). Case-insensitive, so a
// case-folding checkout cannot slip a protected file past a case variant. A
// malformed segment matches: a bad pattern must refuse, not protect nothing.
func matchProtected(pattern, p string) bool {
	return matchSegs(strings.Split(strings.ToLower(pattern), "/"), strings.Split(strings.ToLower(p), "/"))
}

func matchSegs(pat, p []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(p); i++ {
				if matchSegs(pat[1:], p[i:]) {
					return true
				}
			}
			return false
		}
		if len(p) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], p[0]); !ok && err == nil {
			return false
		}
		pat, p = pat[1:], p[1:]
	}
	return len(p) == 0
}
