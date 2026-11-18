package guard

import (
	"context"
	"strings"
)

// A linked worktree shares its repository's `shallow` file, so a shallow
// fetch run from a worktree of the workspace repo cache turns the shared cache
// shallow and breaks every later checkout (CHE-7286). Git has no setting that
// refuses it, so the guard does.
const shallowLine = "git fetch/pull with --depth, --deepen or --shallow-* is refused in a linked worktree: " +
	"it makes the shared repo cache shallow; fetch full history, or run it in an independent clone"

var shallowFlags = []string{"--depth", "--deepen", "--shallow-since", "--shallow-exclude"}

// shallowFetch returns the guard line when the git call would shallow a
// repository shared with other worktrees. It is best effort: an option it does
// not understand, or a failed lookup, lets the call through.
func (e *Engine) shallowFetch(ctx context.Context, args []string) (line string, hit bool) {
	var prefix []string
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		a := args[i]
		prefix = append(prefix, a)
		i++
		if a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree" {
			if i == len(args) {
				return "", false
			}
			prefix = append(prefix, args[i])
			i++
		}
	}
	if i == len(args) || (args[i] != "fetch" && args[i] != "pull") {
		return "", false
	}
	shallow := false
	for _, a := range args[i+1:] {
		if a == "--" {
			break
		}
		for _, f := range shallowFlags {
			if a == f || strings.HasPrefix(a, f+"=") {
				shallow = true
			}
		}
	}
	if !shallow {
		return "", false
	}
	r, err := e.Runner.Run(ctx, "git", append(prefix, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir")...)
	if err != nil || r.Exit != 0 {
		return "", false
	}
	dirs := strings.Fields(r.Stdout)
	if len(dirs) != 2 || dirs[0] == dirs[1] {
		return "", false
	}
	return shallowLine, true
}
