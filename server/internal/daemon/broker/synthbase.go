package broker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// SyntheticBase describes a materialized elevated-run repository (CHE-2216
// plan 3.5, slice B7a).
type SyntheticBase struct {
	Root   string // the synthetic root commit
	Tree   string // tree of the pinned commit
	Source string // the pinned commit whose tree became the root
}

var pinnedSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// PinnedSource picks the commit whose tree seeds an elevated run: the earlier
// task-branch head a member reviewed when the grant names one, otherwise the
// protected-default-branch commit.
func PinnedSource(base, head string) string {
	if head != "" {
		return head
	}
	return base
}

// MaterializeSyntheticBase builds the repository an elevated run sees at dest:
// one root commit (author multica-broker, message "base <sha>", no parent)
// holding the tree of sha, one branch, no remote, no alternates. Only the
// objects reachable from that tree are copied out of cacheGitDir, so upstream
// commits, their messages, other branches and tags never enter the sandbox.
// Time is pinned to the epoch so the same sha always yields the same root.
// dest must not exist or be empty; a failure leaves it as it was found.
func MaterializeSyntheticBase(ctx context.Context, cacheGitDir, sha, dest, branch string) (res SyntheticBase, err error) {
	if !pinnedSHA.MatchString(sha) {
		return res, fmt.Errorf("synthetic base: pinned SHA must be 40 lowercase hex characters")
	}
	if branch == "" || strings.HasPrefix(branch, "-") || !validRef("refs/heads/"+branch) {
		return res, fmt.Errorf("synthetic base: invalid branch name")
	}
	if dest == "" {
		return res, fmt.Errorf("synthetic base: destination required")
	}
	existed, err := emptyOrMissing(dest)
	if err != nil {
		return res, err
	}
	defer func() {
		if err != nil {
			discard(dest, existed)
		}
	}()

	src := "--git-dir=" + cacheGitDir
	commit, err := gitOut(ctx, nil, "", src, "rev-parse", "--verify", "--quiet", sha+"^{commit}")
	if err != nil || commit != sha {
		return res, fmt.Errorf("synthetic base: %s is not a commit in the repository cache", sha)
	}
	tree, err := gitOut(ctx, nil, "", src, "rev-parse", "--verify", sha+"^{tree}")
	if err != nil {
		return res, fmt.Errorf("synthetic base: read tree: %w", err)
	}

	if err = os.MkdirAll(dest, 0o755); err != nil {
		return res, fmt.Errorf("synthetic base: %w", err)
	}
	if _, err = gitOut(ctx, nil, dest, "init", "-q", "--template=", "-b", branch); err != nil {
		return res, err
	}
	if err = copyTree(ctx, cacheGitDir, dest, tree); err != nil {
		return res, err
	}
	author := []string{
		"GIT_AUTHOR_NAME=multica-broker", "GIT_AUTHOR_EMAIL=multica-broker@localhost", "GIT_AUTHOR_DATE=@0 +0000",
		"GIT_COMMITTER_NAME=multica-broker", "GIT_COMMITTER_EMAIL=multica-broker@localhost", "GIT_COMMITTER_DATE=@0 +0000",
	}
	root, err := gitOut(ctx, author, dest, "commit-tree", tree, "-m", "base "+sha)
	if err != nil {
		return res, err
	}
	if _, err = gitOut(ctx, nil, dest, "update-ref", "refs/heads/"+branch, root); err != nil {
		return res, err
	}
	if _, err = gitOut(ctx, nil, dest, "reset", "-q", "--hard", "HEAD"); err != nil {
		return res, err
	}
	return SyntheticBase{Root: root, Tree: tree, Source: sha}, nil
}

// copyTree moves the objects reachable from tree (the tree, its subtrees and
// blobs, never a commit) from the cache into dest as one pack.
func copyTree(ctx context.Context, cacheGitDir, dest, tree string) error {
	list := hostGit(ctx, "", "--git-dir="+cacheGitDir, "rev-list", "--objects", tree)
	pack := hostGit(ctx, "", "--git-dir="+cacheGitDir, "pack-objects", "--stdout", "-q")
	index := hostGit(ctx, dest, "index-pack", "--stdin")
	var errs bytes.Buffer
	list.Stderr, pack.Stderr, index.Stderr = &errs, &errs, &errs
	index.Stdout = io.Discard

	var err error
	if pack.Stdin, err = list.StdoutPipe(); err != nil {
		return err
	}
	if index.Stdin, err = pack.StdoutPipe(); err != nil {
		return err
	}
	for _, c := range []*exec.Cmd{index, pack, list} {
		if err = c.Start(); err != nil {
			return fmt.Errorf("synthetic base: %w", err)
		}
	}
	var first error
	for _, c := range []*exec.Cmd{list, pack, index} {
		if err := c.Wait(); err != nil && first == nil {
			first = err
		}
	}
	if first != nil {
		return fmt.Errorf("synthetic base: copy objects: %w: %s", first, firstLine(errs.String()))
	}
	return nil
}

// hostGit is a git command with host config, hooks, prompts and repository
// selection variables kept out.
func hostGit(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		switch name, _, _ := strings.Cut(kv, "="); name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES":
		default:
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, gitEnv...)
	return cmd
}

// gitOut runs git and returns trimmed stdout. A leading "--git-dir=" argument
// selects the cache; otherwise dir is the working directory.
func gitOut(ctx context.Context, env []string, dir string, args ...string) (string, error) {
	cmd := hostGit(ctx, dir, args...)
	cmd.Env = append(cmd.Env, env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("synthetic base: git %s: %w: %s", firstWord(args), err, firstLine(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func firstWord(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// emptyOrMissing reports whether dest already existed (as an empty directory).
func emptyOrMissing(dest string) (existed bool, err error) {
	entries, err := os.ReadDir(dest)
	switch {
	case os.IsNotExist(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("synthetic base: %w", err)
	case len(entries) > 0:
		return true, fmt.Errorf("synthetic base: destination %s is not empty", dest)
	}
	return true, nil
}

// discard undoes a failed build: remove dest, or only its contents when the
// caller handed over a directory that already existed.
func discard(dest string, existed bool) {
	if !existed {
		_ = os.RemoveAll(dest)
		return
	}
	entries, _ := os.ReadDir(dest)
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dest, e.Name()))
	}
}

// DiskUsage is the apparent size of dir and everything under it, the number
// `du -sb` reports; it is the M5 measure.
func DiskUsage(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
