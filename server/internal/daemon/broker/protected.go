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
	MessageSHA256 string `json:"message_sha256"`
}

// Receipt records one successful push (plan 3.2 item 7). The read proxy uses
// it to return the run's own commit messages.
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
func (b *Broker) defaultBranch(ctx context.Context) (string, error) {
	out, err := b.git(ctx, "", "ls-remote", "--symref", b.pin.RemoteURL, "HEAD")
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
// remote default branch at any path the policy protects. The net tree diff is
// what a merge would land; renames are split so both sides are checked.
func (b *Broker) checkProtected(ctx context.Context, work, defRef, sha string, patterns []string) (int, error) {
	const def = "refs/multica/default"
	if out, err := b.git(ctx, work, "-c", "transfer.fsckObjects=true", "fetch", "--no-tags", "--quiet", b.pin.RemoteURL, "+"+defRef+":"+def); err != nil {
		return http.StatusServiceUnavailable, fmt.Errorf("default branch fetch: %s", b.scrub(firstLine(out)))
	}
	bases, err := b.git(ctx, work, "merge-base", "--all", def, sha)
	if err != nil || bases == "" {
		return http.StatusForbidden, errors.New("the pushed branch shares no history with the default branch")
	}
	for _, base := range strings.Fields(bases) {
		raw, err := exec.CommandContext(ctx, "git", "--git-dir", work, "diff", "--name-only", "-z", "--no-renames", base, sha).Output()
		if err != nil {
			return http.StatusInternalServerError, fmt.Errorf("diff: %w", err)
		}
		for _, p := range strings.Split(string(raw), "\x00") {
			for _, pat := range patterns {
				if p != "" && matchProtected(pat, p) {
					return http.StatusForbidden, fmt.Errorf("change to protected path %q (policy %q)", p, pat)
				}
			}
		}
	}
	return 0, nil
}

// commitReceipts lists the commits the bundle added on top of the pinned base
// with the SHA-256 of each raw message, using one git process for all objects.
func (b *Broker) commitReceipts(ctx context.Context, work, sha string) ([]Commit, error) {
	list, err := b.git(ctx, work, "rev-list", "--reverse", sha, "^"+b.pin.Base)
	if err != nil {
		return nil, fmt.Errorf("receipt: %s", firstLine(list))
	}
	if list == "" {
		return nil, nil
	}
	cmd := exec.CommandContext(ctx, "git", "--git-dir", work, "cat-file", "--batch")
	cmd.Stdin = strings.NewReader(list + "\n")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("receipt: %w", err)
	}
	r := bufio.NewReader(bytes.NewReader(out))
	var commits []Commit
	for _, want := range strings.Fields(list) {
		hdr, err := r.ReadString('\n')
		f := strings.Fields(hdr)
		if err != nil || len(f) != 3 || f[0] != want || f[1] != "commit" {
			return nil, fmt.Errorf("receipt: unexpected object header %q", hdr)
		}
		n, _ := strconv.Atoi(f[2])
		obj := make([]byte, n+1) // object bytes and the trailing newline
		if _, err := io.ReadFull(r, obj); err != nil {
			return nil, fmt.Errorf("receipt: %w", err)
		}
		var msg []byte
		if _, m, ok := bytes.Cut(obj[:n], []byte("\n\n")); ok {
			msg = m
		}
		sum := sha256.Sum256(msg)
		commits = append(commits, Commit{SHA: want, MessageSHA256: hex.EncodeToString(sum[:])})
	}
	return commits, nil
}

// matchProtected reports whether path falls under a policy pattern: slash
// separated segments, `*` within a segment, `**` for any number of whole
// segments (a trailing `**` needs at least one). Case-insensitive, so a
// case-folding checkout cannot slip a protected file past a case variant. A
// malformed segment matches: a bad pattern must refuse, not protect nothing.
func matchProtected(pattern, p string) bool {
	return matchSegs(strings.Split(strings.ToLower(pattern), "/"), strings.Split(strings.ToLower(p), "/"))
}

func matchSegs(pat, p []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return len(p) > 0
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
