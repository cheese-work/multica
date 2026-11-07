package broker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

var errCommitTooLarge = errors.New("push contains an oversized commit")

var errNotTranslatable = errors.New("push history does not start at the synthetic base")

// epochIdent pins the synthetic root's author and time, as MaterializeSyntheticBase does.
var epochIdent = []string{
	"GIT_AUTHOR_NAME=multica-broker", "GIT_AUTHOR_EMAIL=multica-broker@localhost", "GIT_AUTHOR_DATE=@0 +0000",
	"GIT_COMMITTER_NAME=multica-broker", "GIT_COMMITTER_EMAIL=multica-broker@localhost", "GIT_COMMITTER_DATE=@0 +0000",
}

// seedRoot recreates the synthetic root in the scratch repository so the run's
// bundle, which lists it as a prerequisite, unbundles. The root is deterministic
// from the real parent's tree; a different result means the pin is wrong.
func (b *Broker) seedRoot(ctx context.Context, work string) (int, error) {
	s := b.pin.Synthetic
	tree, err := b.git(ctx, work, nil, "rev-parse", "--verify", s.Parent+"^{tree}")
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("synthetic base: parent tree: %s", firstLine(tree))
	}
	root, err := b.git(ctx, work, epochIdent, "commit-tree", tree, "-m", "base "+s.Parent)
	if err != nil || root != s.Root {
		return http.StatusInternalServerError, errors.New("synthetic base: root does not match its real parent")
	}
	return 0, nil
}

// translate rewrites the commits sha adds on top of the synthetic root with
// git commit-tree, mapping the root to the real parent and keeping tree,
// author, committer and message (plan 3.5). It returns the translated tip and,
// for each translated commit, the SHA the run knew it by.
// Every commit must descend from the root: a second root, or a parent outside
// the pushed set, refuses the push, so no run commit reaches the real history
// by any other path.
func (b *Broker) translate(ctx context.Context, work, sha, msgFile string) (string, map[string]string, int, error) {
	s := b.pin.Synthetic
	limit := b.pin.MaxCommits
	if limit <= 0 {
		limit = defaultMaxCommits
	}
	list, err := b.git(ctx, work, nil, "rev-list", "--reverse", "--topo-order", "--max-count="+strconv.Itoa(limit+1), sha, "^"+s.Root)
	if err != nil {
		return "", nil, http.StatusInternalServerError, fmt.Errorf("translate: %s", firstLine(list))
	}
	shas := strings.Fields(list)
	if len(shas) > limit {
		return "", nil, http.StatusRequestEntityTooLarge, errTooManyCommits
	}
	mapped := map[string]string{s.Root: s.Parent}
	runSHA := make(map[string]string, len(shas))
	for _, old := range shas {
		// The size comes from the object header, so a compressed bomb is
		// refused before its decompressed bytes reach daemon memory.
		size, err := b.git(ctx, work, nil, "cat-file", "-s", old)
		if err != nil {
			return "", nil, http.StatusInternalServerError, fmt.Errorf("translate: %s", firstLine(size))
		}
		if n, err := strconv.Atoi(size); err != nil || n > maxCommitBytes {
			return "", nil, http.StatusRequestEntityTooLarge, errCommitTooLarge
		}
		raw, err := rawGit(ctx, work, nil, "cat-file", "commit", old)
		if err != nil {
			return "", nil, http.StatusInternalServerError, fmt.Errorf("translate: %w", err)
		}
		args, env, msg, ok := parseCommit(raw, mapped)
		if !ok {
			return "", nil, http.StatusBadRequest, errNotTranslatable
		}
		if err := os.WriteFile(msgFile, msg, 0o600); err != nil {
			return "", nil, http.StatusInternalServerError, err
		}
		out, err := b.git(ctx, work, env, append(args, "-F", msgFile)...)
		if err != nil {
			return "", nil, http.StatusBadRequest, fmt.Errorf("translate: %s", firstLine(out))
		}
		mapped[old] = out
		runSHA[out] = old
	}
	tip, ok := mapped[sha]
	if !ok || sha == s.Root {
		return "", nil, http.StatusBadRequest, errNotTranslatable
	}
	return tip, runSHA, 0, nil
}

// parseCommit reads a raw commit object into commit-tree arguments and author
// and committer environment, with each parent looked up in mapped. Other
// headers (signatures, encoding) are dropped: they cannot survive a rewrite.
func parseCommit(raw []byte, mapped map[string]string) (args, env []string, msg []byte, ok bool) {
	head, body, _ := strings.Cut(string(raw), "\n\n")
	var tree string
	var parents []string
	for _, l := range strings.Split(head, "\n") {
		key, val, _ := strings.Cut(l, " ")
		switch key {
		case "tree":
			tree = val
		case "parent":
			p, found := mapped[val]
			if !found {
				return nil, nil, nil, false
			}
			parents = append(parents, "-p", p)
		case "author", "committer":
			i := strings.LastIndex(val, "> ")
			j := strings.Index(val, " <")
			if i < 0 || j < 0 || j > i {
				return nil, nil, nil, false
			}
			p := "GIT_" + strings.ToUpper(key) + "_"
			env = append(env, p+"NAME="+val[:j], p+"EMAIL="+val[j+2:i], p+"DATE=@"+val[i+2:])
		}
	}
	if tree == "" || len(env) != 6 || len(parents) == 0 {
		// A commit with no parent is a second root.
		return nil, nil, nil, false
	}
	return append([]string{"commit-tree", tree}, parents...), env, []byte(body), true
}

// rawGit runs git in the bare repository work and returns stdout untrimmed.
func rawGit(ctx context.Context, work string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir", work}, args...)...)
	cmd.Env = append(append(os.Environ(), gitEnv...), env...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", firstWord(args), err)
	}
	return out, nil
}
