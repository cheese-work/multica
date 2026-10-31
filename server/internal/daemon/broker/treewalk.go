package broker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"
)

// The protected-path check walks tree objects itself instead of asking git for
// a recursive diff: git queues every changed path in memory, and one compact
// commit can reuse a subtree under millions of names. The walk counts what it
// expands and stops at a cap, so the cost is bounded by the cap, not the push.
const (
	defaultMaxPaths = 100_000
	maxTreeBytes    = 4 << 20 // one tree object
	maxTreeDepth    = 256
	maxLinkTarget   = 4096
	maxReadBytes    = 256 << 20 // all objects of one check
	maxLinks        = 10_000
	maxAliases      = 1000 // per changed path
	maxLinkChain    = 40   // links followed in a row; the kernel stops at 40 too
	modeTree        = "40000"
	modeSymlink     = "120000"
)

var (
	errBudget = errors.New("push changes too many paths to check")
	errTooBig = errors.New("object too big")
	// errRefuse marks a push the check will not reason about: it is refused
	// outright rather than guessed at.
	errRefuse = errors.New("push refused")
)

// canonicalModes are the only tree entry modes git itself writes. A raw mode
// such as 040000 names a directory but is not the string the walk compares.
var canonicalModes = map[string]bool{modeTree: true, "100644": true, "100755": true, modeSymlink: true, "160000": true}

type treeEntry struct{ mode, name, oid string }

func (e treeEntry) isTree() bool { return e.mode == modeTree }

// objReader asks one `git cat-file --batch` process for objects, one at a time.
type objReader struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	reads  int
	budget int
	bytes  int64
}

func (b *Broker) newObjReader(ctx context.Context, work string, budget int) (*objReader, error) {
	cmd := exec.CommandContext(ctx, "git", "--git-dir", work, "cat-file", "--batch")
	cmd.Env = append(os.Environ(), gitEnv...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &objReader{cmd: cmd, in: in, out: bufio.NewReader(out), budget: budget}, nil
}

func (o *objReader) close() {
	_ = o.in.Close()
	_ = o.cmd.Process.Kill()
	_ = o.cmd.Wait()
}

// read returns the body of an object of the wanted type, at most max bytes.
func (o *objReader) read(oid, typ string, max int64) ([]byte, error) {
	if o.reads++; o.reads > o.budget {
		return nil, errBudget
	}
	if _, err := io.WriteString(o.in, oid+"\n"); err != nil {
		return nil, err
	}
	hdr, err := o.out.ReadString('\n')
	f := strings.Fields(hdr)
	if err != nil || len(f) != 3 || f[0] != oid || f[1] != typ {
		return nil, fmt.Errorf("unexpected object header %q", hdr)
	}
	n, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("unexpected object size %q", f[2])
	}
	if o.bytes += n; o.bytes > maxReadBytes {
		return nil, errBudget
	}
	if n > max { // keep the stream in step: drop the body and its newline
		if _, err := io.CopyN(io.Discard, o.out, n+1); err != nil {
			return nil, err
		}
		return nil, errTooBig
	}
	body := make([]byte, n+1) // object bytes and the trailing newline
	if _, err := io.ReadFull(o.out, body); err != nil {
		return nil, err
	}
	return body[:n], nil
}

// tree parses one tree object; an empty oid is the absent tree.
func (o *objReader) tree(oid string) ([]treeEntry, error) {
	if oid == "" {
		return nil, nil
	}
	body, err := o.read(oid, "tree", maxTreeBytes)
	if errors.Is(err, errTooBig) {
		return nil, errBudget
	}
	if err != nil {
		return nil, err
	}
	rawLen := len(oid) / 2
	var es []treeEntry
	names := map[string]bool{}
	for len(body) > 0 {
		sp := bytes.IndexByte(body, ' ')
		nul := bytes.IndexByte(body, 0)
		if sp < 0 || nul < sp || len(body) < nul+1+rawLen {
			return nil, errors.New("malformed tree")
		}
		e := treeEntry{mode: string(body[:sp]), name: string(body[sp+1 : nul]), oid: fmt.Sprintf("%x", body[nul+1:nul+1+rawLen])}
		if !canonicalModes[e.mode] {
			return nil, fmt.Errorf("%w: tree entry %q has non-canonical mode %q", errRefuse, e.name, e.mode)
		}
		if names[e.name] {
			return nil, fmt.Errorf("%w: tree lists %q twice", errRefuse, e.name)
		}
		names[e.name] = true
		es = append(es, e)
		body = body[nul+1+rawLen:]
	}
	return es, nil
}

// pathSink receives each changed path; a non-nil error stops the walk.
type pathSink func(p string) error

type walker struct {
	o     *objReader
	sink  pathSink
	paths int
	max   int
}

// emit reports one changed path with the entries it was before and after. A
// change to a symlink is refused: where a link leads is not something this
// check resolves.
func (w *walker) emit(p string, es ...treeEntry) error {
	for _, e := range es {
		if e.mode == modeSymlink {
			return fmt.Errorf("%w: push adds, changes or removes the symlink %q", errRefuse, p)
		}
	}
	if w.paths++; w.paths > w.max {
		return errBudget
	}
	return w.sink(p)
}

// diff emits every non-tree path that differs between two trees.
func (w *walker) diff(base, tip, prefix string, depth int) error {
	if base == tip {
		return nil
	}
	if depth > maxTreeDepth {
		return errBudget
	}
	be, err := w.o.tree(base)
	if err != nil {
		return err
	}
	te, err := w.o.tree(tip)
	if err != nil {
		return err
	}
	old := make(map[string]treeEntry, len(be))
	for _, e := range be {
		old[e.name] = e
	}
	seen := make(map[string]bool, len(te))
	for _, t := range te {
		seen[t.name] = true
		b, had := old[t.name]
		if had && b == t {
			continue
		}
		p := prefix + t.name
		switch {
		case had && b.isTree() && t.isTree():
			err = w.diff(b.oid, t.oid, p+"/", depth+1)
		case t.isTree():
			if had { // a file became a directory: the path itself changed too
				err = w.emit(p, b)
			}
			if err == nil {
				err = w.all(t.oid, p+"/", depth+1)
			}
		default:
			err = w.emit(p, t, b)
			if err == nil && had && b.isTree() {
				err = w.all(b.oid, p+"/", depth+1)
			}
		}
		if err != nil {
			return err
		}
	}
	for _, b := range be {
		if seen[b.name] {
			continue
		}
		if b.isTree() {
			err = w.all(b.oid, prefix+b.name+"/", depth+1)
		} else {
			err = w.emit(prefix+b.name, b)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// all emits every non-tree path under a whole tree (added or removed).
func (w *walker) all(oid, prefix string, depth int) error {
	if depth > maxTreeDepth {
		return errBudget
	}
	es, err := w.o.tree(oid)
	if err != nil {
		return err
	}
	for _, e := range es {
		if e.isTree() {
			err = w.all(e.oid, prefix+e.name+"/", depth+1)
		} else {
			err = w.emit(prefix+e.name, e)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// symlink is one symlink in the pushed tree and where it points, both
// lower-cased and relative to the repository root ("" is the root itself).
type symlink struct{ at, to string }

// symlinks lists the symlinks of a tree. Subtrees are memoised by object ID, so
// a subtree reused under many names is read once; the number of links returned
// is capped. A link is accepted only if its target is a plain in-tree path: not
// empty, not absolute, no ".." step, and no step before the last one is itself
// a link. Then the target needs no resolution, only a prefix match. Any other
// link refuses the push; the check does not emulate the file system.
func (w *walker) symlinks(root string) ([]symlink, error) {
	memo := map[string][]treeLink{}
	links, err := w.treeLinks(root, memo, 0)
	if err != nil {
		return nil, err
	}
	isLink := make(map[string]bool, len(links))
	for _, l := range links {
		isLink[strings.ToLower(l.at)] = true
	}
	var out []symlink
	blobs := map[string]string{}
	for _, l := range links {
		target, ok := blobs[l.blob]
		if !ok {
			body, err := w.o.read(l.blob, "blob", maxLinkTarget)
			if errors.Is(err, errTooBig) {
				return nil, fmt.Errorf("%w: symlink %q has an over-long target", errRefuse, l.at)
			}
			if err != nil {
				return nil, err
			}
			target = string(body)
			blobs[l.blob] = target
		}
		if target == "" || strings.HasPrefix(target, "/") || slices.Contains(strings.Split(target, "/"), "..") {
			return nil, fmt.Errorf("%w: symlink %q has an empty, absolute or \"..\" target", errRefuse, l.at)
		}
		to := path.Join(path.Dir(l.at), target)
		if to == "." {
			to = ""
		}
		to = strings.ToLower(to)
		for i := 0; i < len(to); i++ {
			if to[i] == '/' && isLink[to[:i]] {
				return nil, fmt.Errorf("%w: symlink %q passes through the symlink %q", errRefuse, l.at, to[:i])
			}
		}
		out = append(out, symlink{at: strings.ToLower(l.at), to: to})
	}
	return out, nil
}

type treeLink struct{ at, blob string }

func (w *walker) treeLinks(oid string, memo map[string][]treeLink, depth int) ([]treeLink, error) {
	if got, ok := memo[oid]; ok {
		return got, nil
	}
	if depth > maxTreeDepth {
		return nil, errBudget
	}
	es, err := w.o.tree(oid)
	if err != nil {
		return nil, err
	}
	var out []treeLink
	for _, e := range es {
		switch {
		case e.mode == modeSymlink:
			out = append(out, treeLink{at: e.name, blob: e.oid})
		case e.isTree():
			sub, err := w.treeLinks(e.oid, memo, depth+1)
			if err != nil {
				return nil, err
			}
			for _, l := range sub {
				out = append(out, treeLink{at: e.name + "/" + l.at, blob: l.blob})
			}
		}
		if len(out) > maxLinks {
			return nil, errBudget
		}
	}
	memo[oid] = out
	return out, nil
}

// linkIndex finds the links that point at a given directory or file.
type linkIndex map[string][]string

func indexLinks(links []symlink) linkIndex {
	idx := linkIndex{}
	for _, l := range links {
		idx[l.to] = append(idx[l.to], l.at)
	}
	return idx
}

// aliases returns every path through which a change at p shows up: p itself
// and, for each symlink that points at p or at a directory above it, the path
// under that link, and so on through chains of links. The result is capped.
func (idx linkIndex) aliases(p string) ([]string, error) {
	type item struct {
		path  string
		depth int
	}
	p = strings.ToLower(p)
	seen := map[string]bool{p: true}
	queue := []item{{p, 0}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth >= maxLinkChain {
			continue
		}
		// Every directory prefix of the path, longest first, then the root.
		for prefix := cur.path; ; {
			for _, at := range idx[prefix] {
				alias := at + "/" + cur.path[len(prefix):]
				if prefix != "" {
					alias = at + cur.path[len(prefix):]
				}
				if seen[alias] {
					continue
				}
				if len(seen) >= maxAliases {
					return nil, errBudget
				}
				seen[alias] = true
				queue = append(queue, item{alias, cur.depth + 1})
			}
			if prefix == "" {
				break
			}
			i := strings.LastIndexByte(prefix, '/')
			if i < 0 {
				prefix = ""
			} else {
				prefix = prefix[:i]
			}
		}
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	return out, nil
}
