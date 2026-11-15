package configsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const agentUUID = "11111111-2222-3333-4444-555555555555"

func digest(b string) string {
	sum := sha256.Sum256([]byte(b))
	return hex.EncodeToString(sum[:])
}

type fakeGH struct {
	pr       PullRequest
	timeline []string
	prMsgs   []string
	commits  map[string]Commit
	files    map[string]string // "ref:path" -> content
	fetchErr map[string]error  // "ref:path" -> error
}

func (f *fakeGH) PullRequest(context.Context, int) (PullRequest, error) { return f.pr, nil }
func (f *fakeGH) TimelineEvents(context.Context, int) ([]string, error) { return f.timeline, nil }
func (f *fakeGH) PRCommitMessages(context.Context, int) ([]string, error) {
	return f.prMsgs, nil
}
func (f *fakeGH) Commit(_ context.Context, sha string) (Commit, error) {
	c, ok := f.commits[sha]
	if !ok {
		return Commit{}, errors.New("no commit " + sha)
	}
	return c, nil
}
func (f *fakeGH) File(_ context.Context, ref, path string) ([]byte, bool, error) {
	if err := f.fetchErr[ref+":"+path]; err != nil {
		return nil, false, err
	}
	v, ok := f.files[ref+":"+path]
	return []byte(v), ok, nil
}

type fakeTargets map[string]string // target -> live value; absent = not in workspace

func (f fakeTargets) LiveDigest(_ context.Context, t string) (string, bool, error) {
	v, ok := f[t]
	return digest(v), ok, nil
}

func manifest(entries ...string) string {
	return `{"entries":[` + strings.Join(entries, ",") + `]}`
}
func entry(target, file string) string {
	return fmt.Sprintf(`{"target":%q,"file":%q}`, target, file)
}

var settings = Settings{Repo: "o/r", DefaultBranch: "main", ManifestPath: "m.json", OwnerLogins: []string{"Owner", "owner[bot]"}}

// base is a direct owner merge of one agent file: pre-merge commit "pre",
// merge commit "mrg" (a two-parent merge commit).
func base() *fakeGH {
	return &fakeGH{
		pr:      PullRequest{Merged: true, MergedByLogin: "owner", MergedByType: "User", BaseRef: "main", MergeCommitSHA: "mrg"},
		commits: map[string]Commit{"mrg": {Parents: []string{"pre", "head"}}},
		files: map[string]string{
			"pre:m.json": manifest(entry("agent:"+agentUUID, "a.md")),
			"mrg:m.json": manifest(entry("agent:"+agentUUID, "a.md")),
			"pre:a.md":   "old",
			"mrg:a.md":   "new",
		},
	}
}

func eval(gh *fakeGH, tg fakeTargets) Receipt {
	return Evaluate(context.Background(), gh, tg, settings, 7)
}

func only(t *testing.T, r Receipt) TargetResult {
	t.Helper()
	if r.Skipped != "" || len(r.Targets) != 1 {
		t.Fatalf("want one target result, got skipped=%q targets=%+v", r.Skipped, r.Targets)
	}
	return r.Targets[0]
}

func TestOwnerMergeWouldApply(t *testing.T) {
	r := eval(base(), fakeTargets{"agent:" + agentUUID: "old"})
	got := only(t, r)
	if got.Result != "would apply" || got.FileDigest != digest("new") || r.PreMergeSHA != "pre" {
		t.Fatalf("got %+v pre=%s", got, r.PreMergeSHA)
	}
}

func TestLiveStateResults(t *testing.T) {
	for live, want := range map[string]string{"new": "in sync", "edited": "drift"} {
		got := only(t, eval(base(), fakeTargets{"agent:" + agentUUID: live}))
		if got.Result != want {
			t.Errorf("live=%q: got %q want %q", live, got.Result, want)
		}
	}
}

func TestPRLevelSkips(t *testing.T) {
	cases := map[string]func(*fakeGH){
		"skipped: not merged":        func(g *fakeGH) { g.pr.Merged = false },
		"skipped: wrong base branch": func(g *fakeGH) { g.pr.BaseRef = "dev" },
		"skipped: not owner merge":   func(g *fakeGH) { g.pr.MergedByLogin = "stranger" },
		"skipped: not direct merge (bot)": func(g *fakeGH) {
			g.pr.MergedByLogin, g.pr.MergedByType = "Owner", "Bot"
		},
		"skipped: not direct merge (bot login)": func(g *fakeGH) { g.pr.MergedByLogin = "owner[bot]" },
		"skipped: not direct merge (auto)":      func(g *fakeGH) { g.timeline = []string{"commented", "auto_merge_enabled"} },
		"skipped: not direct merge (queue)":     func(g *fakeGH) { g.timeline = []string{"added_to_merge_queue"} },
		"skipped: invalid manifest (pre)":       func(g *fakeGH) { g.files["pre:m.json"] = "{" },
		"skipped: invalid manifest (merged)":    func(g *fakeGH) { g.files["mrg:m.json"] = `{"entries":[{"target":"workspace.context"}]}` },
	}
	for name, mutate := range cases {
		g := base()
		mutate(g)
		r := eval(g, fakeTargets{"agent:" + agentUUID: "old"})
		want := strings.SplitN(name, " (", 2)[0]
		if r.Skipped != want || len(r.Targets) != 0 {
			t.Errorf("%s: got skipped=%q targets=%+v", name, r.Skipped, r.Targets)
		}
	}
}

func TestOwnerLoginMatchIsCaseInsensitiveAndExact(t *testing.T) {
	g := base()
	g.pr.MergedByLogin = "OWNER"
	if r := eval(g, fakeTargets{"agent:" + agentUUID: "old"}); r.Skipped != "" {
		t.Fatalf("case-insensitive owner rejected: %q", r.Skipped)
	}
	g.pr.MergedByLogin = "owner2"
	if r := eval(g, fakeTargets{}); r.Skipped != "skipped: not owner merge" {
		t.Fatalf("got %q", r.Skipped)
	}
}

func TestPreMergeManifestDecidesTargets(t *testing.T) {
	g := base()
	other := "99999999-2222-3333-4444-555555555555"
	// The PR's own manifest edit adds a target; it must not become a write target.
	g.files["mrg:m.json"] = manifest(entry("agent:"+agentUUID, "a.md"), entry("agent:"+other, "b.md"))
	g.files["mrg:b.md"] = "x"
	r := eval(g, fakeTargets{"agent:" + agentUUID: "old", "agent:" + other: "x"})
	if len(r.Targets) != 2 {
		t.Fatalf("got %+v", r.Targets)
	}
	byT := map[string]string{}
	for _, tr := range r.Targets {
		byT[tr.Target] = tr.Result
	}
	if byT["agent:"+agentUUID] != "would apply" || byT["agent:"+other] != "onboarding (no write in L1)" {
		t.Fatalf("got %+v", byT)
	}
}

func TestUnchangedFileIsIgnored(t *testing.T) {
	g := base()
	g.files["mrg:a.md"] = "old"
	r := eval(g, fakeTargets{"agent:" + agentUUID: "old"})
	if r.Skipped != "" || len(r.Targets) != 0 {
		t.Fatalf("got %+v", r)
	}
}

func TestTargetChecks(t *testing.T) {
	cases := map[string]struct {
		target string
		live   fakeTargets
		want   string
	}{
		"squad type":    {"squad:" + agentUUID, fakeTargets{}, "skipped: target type"},
		"bad uuid":      {"agent:not-a-uuid", fakeTargets{}, "skipped: target type"},
		"project":       {"project:" + agentUUID, fakeTargets{}, "skipped: target type"},
		"other ws":      {"agent:" + agentUUID, fakeTargets{}, "skipped: target not in workspace"},
		"ws context ok": {"workspace.context", fakeTargets{"workspace.context": "old"}, "would apply"},
	}
	for name, c := range cases {
		g := base()
		g.files["pre:m.json"] = manifest(entry(c.target, "a.md"))
		g.files["mrg:m.json"] = g.files["pre:m.json"]
		got := only(t, eval(g, c.live))
		if got.Result != c.want {
			t.Errorf("%s: got %q want %q", name, got.Result, c.want)
		}
	}
}

func TestDeletedFileNeverWrites(t *testing.T) {
	g := base()
	delete(g.files, "mrg:a.md")
	got := only(t, eval(g, fakeTargets{"agent:" + agentUUID: "old"}))
	if got.Result != "skipped: deleted" {
		t.Fatalf("got %q", got.Result)
	}
}

func TestSizeCapIsInCharactersPerSurface(t *testing.T) {
	g := base()
	g.files["mrg:a.md"] = strings.Repeat("é", 4000) // 4000 chars, 8000 bytes: allowed
	if got := only(t, eval(g, fakeTargets{"agent:" + agentUUID: "old"})); got.Result != "would apply" {
		t.Fatalf("at cap: got %q", got.Result)
	}
	g.files["mrg:a.md"] = strings.Repeat("a", 4001)
	if got := only(t, eval(g, fakeTargets{"agent:" + agentUUID: "old"})); got.Result != "skipped: over cap" {
		t.Fatalf("over agent cap: got %q", got.Result)
	}
	g.files["pre:m.json"] = manifest(entry("workspace.context", "a.md"))
	g.files["mrg:m.json"] = g.files["pre:m.json"]
	g.files["mrg:a.md"] = strings.Repeat("a", 10000)
	if got := only(t, eval(g, fakeTargets{"workspace.context": "old"})); got.Result != "would apply" {
		t.Fatalf("workspace cap: got %q", got.Result)
	}
	g.files["mrg:a.md"] = strings.Repeat("a", 10001)
	if got := only(t, eval(g, fakeTargets{"workspace.context": "old"})); got.Result != "skipped: over cap" {
		t.Fatalf("over workspace cap: got %q", got.Result)
	}
}

func TestInvalidBytesSkip(t *testing.T) {
	for name, content := range map[string]string{"nul": "a\x00b", "utf8": "a\xffb"} {
		g := base()
		g.files["mrg:a.md"] = content
		if got := only(t, eval(g, fakeTargets{"agent:" + agentUUID: "old"})); got.Result != "skipped: invalid encoding" {
			t.Errorf("%s: got %q", name, got.Result)
		}
	}
}

func TestFetchErrorFailsTheTarget(t *testing.T) {
	g := base()
	g.fetchErr = map[string]error{"mrg:a.md": errors.New("boom")}
	if got := only(t, eval(g, fakeTargets{"agent:" + agentUUID: "old"})); got.Result != "failed: fetch" {
		t.Fatalf("got %q", got.Result)
	}
	g = base()
	g.fetchErr = map[string]error{"pre:m.json": errors.New("boom")}
	if r := eval(g, fakeTargets{}); r.Skipped != "failed: fetch" {
		t.Fatalf("manifest fetch: got %q", r.Skipped)
	}
}

func TestPreMergeCommitPerMergeMethod(t *testing.T) {
	// squash: one parent, PR had 3 commits whose messages differ from the squash commit.
	g := base()
	g.commits = map[string]Commit{"mrg": {Parents: []string{"pre"}, Message: "title (#7)"}, "pre": {Parents: []string{"p0"}, Message: "x"}, "p0": {Message: "y"}}
	g.prMsgs = []string{"a", "b", "c"}
	if r := eval(g, fakeTargets{"agent:" + agentUUID: "old"}); r.PreMergeSHA != "pre" {
		t.Fatalf("squash: got %q", r.PreMergeSHA)
	}
	// rebase: three rebased commits c1<-c2<-c3(mrg) on top of pre, same messages as the PR's.
	g = base()
	g.commits = map[string]Commit{
		"mrg": {Parents: []string{"r2"}, Message: "c"},
		"r2":  {Parents: []string{"r1"}, Message: "b"},
		"r1":  {Parents: []string{"pre"}, Message: "a"},
		"pre": {Parents: []string{"p0"}, Message: "base"},
	}
	g.prMsgs = []string{"a", "b", "c"}
	if r := eval(g, fakeTargets{"agent:" + agentUUID: "old"}); r.PreMergeSHA != "pre" {
		t.Fatalf("rebase: got %q", r.PreMergeSHA)
	}
	// single-commit rebase equals squash: first parent.
	g = base()
	g.commits = map[string]Commit{"mrg": {Parents: []string{"pre"}, Message: "a"}}
	g.prMsgs = []string{"a"}
	if r := eval(g, fakeTargets{"agent:" + agentUUID: "old"}); r.PreMergeSHA != "pre" {
		t.Fatalf("single: got %q", r.PreMergeSHA)
	}
	// unresolvable history fails closed.
	g = base()
	g.commits = map[string]Commit{}
	if r := eval(g, fakeTargets{}); r.Skipped != "failed: fetch" {
		t.Fatalf("unresolvable: got %q", r.Skipped)
	}
}

func TestDigestIsPlainSHA256OfRawBytes(t *testing.T) {
	// No newline normalisation: a trailing LF is part of the digest input.
	if Digest([]byte("x\n")) == Digest([]byte("x")) {
		t.Fatal("trailing LF must change the digest")
	}
	want := sha256.Sum256([]byte("x\n"))
	if Digest([]byte("x\n")) != hex.EncodeToString(want[:]) {
		t.Fatal("digest is not lowercase hex sha256 of the raw bytes")
	}
}

func TestMixedResultsRenderOneLinePerTarget(t *testing.T) {
	r := Receipt{PR: 7, MergeSHA: "mrg", Targets: []TargetResult{
		{Target: "agent:" + agentUUID, File: "a.md", Result: "would apply", FileDigest: "d1"},
		{Target: "workspace.context", File: "w.md", Result: "skipped: over cap"},
	}}
	out := RenderComment(r)
	if !strings.Contains(out, "<!-- config-sync dry-run mrg -->") || !strings.Contains(out, "Nothing was written") ||
		strings.Count(out, "\n| `") != 2 {
		t.Fatalf("unexpected comment:\n%s", out)
	}
}

func TestParseSettings(t *testing.T) {
	ok := `{"repo":"o/r","default_branch":"main","manifest_path":"config/manifest.json","owner_logins":["a"]}`
	if s, err := ParseSettings(ok); err != nil || s.Repo != "o/r" || s.OwnerLogins[0] != "a" {
		t.Fatalf("valid settings: %+v %v", s, err)
	}
	bad := map[string]string{
		"not json":         `x`,
		"unknown field":    `{"repo":"o/r","default_branch":"main","manifest_path":"m.json","owner_logins":["a"],"x":1}`,
		"trailing data":    ok + `{}`,
		"repo shape":       `{"repo":"o","default_branch":"main","manifest_path":"m.json","owner_logins":["a"]}`,
		"repo traversal":   `{"repo":"o/..","default_branch":"main","manifest_path":"m.json","owner_logins":["a"]}`,
		"empty branch":     `{"repo":"o/r","default_branch":"","manifest_path":"m.json","owner_logins":["a"]}`,
		"absolute path":    `{"repo":"o/r","default_branch":"main","manifest_path":"/m.json","owner_logins":["a"]}`,
		"dotdot path":      `{"repo":"o/r","default_branch":"main","manifest_path":"../m.json","owner_logins":["a"]}`,
		"unclean path":     `{"repo":"o/r","default_branch":"main","manifest_path":"a//m.json","owner_logins":["a"]}`,
		"no owners":        `{"repo":"o/r","default_branch":"main","manifest_path":"m.json","owner_logins":[]}`,
		"bad login":        `{"repo":"o/r","default_branch":"main","manifest_path":"m.json","owner_logins":["a b"]}`,
		"too many owners":  `{"repo":"o/r","default_branch":"main","manifest_path":"m.json","owner_logins":["a","b","c","d","e","f","g","h","i","j","k"]}`,
		"duplicate owners": `{"repo":"o/r","default_branch":"main","manifest_path":"m.json","owner_logins":["a","A"]}`,
	}
	for name, in := range bad {
		if _, err := ParseSettings(in); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

// The pre-merge boundary decides which mappings are trusted. An unreadable or
// ambiguous history must fail closed, never fall back to the merge commit's
// parent: inside a rebased PR that parent is a PR commit, so a mapping the PR
// added in its first commit would be trusted as pre-merge.
func TestPreMergeBoundaryFailsClosedWhenHistoryIsUnreadable(t *testing.T) {
	rebased := func() *fakeGH {
		g := base()
		g.commits = map[string]Commit{
			"mrg": {Parents: []string{"r2"}, Message: "c"},
			"r2":  {Parents: []string{"r1"}, Message: "b"},
			"r1":  {Parents: []string{"pre"}, Message: "a"},
			"pre": {Parents: []string{"p0"}, Message: "base"},
		}
		g.prMsgs = []string{"a", "b", "c"}
		return g
	}
	for _, missing := range []string{"r2", "r1"} {
		g := rebased()
		delete(g.commits, missing)
		if r := eval(g, fakeTargets{"agent:" + agentUUID: "old"}); r.Skipped != "failed: fetch" || len(r.Targets) != 0 {
			t.Errorf("read failure at %s: want failed: fetch, got skipped=%q targets=%v", missing, r.Skipped, r.Targets)
		}
	}

	// More PR commits than the walk limit: cannot be proven, so no guess.
	g := rebased()
	for _, n := range []int{maxRebaseWalk, maxRebaseWalk + 1} {
		g.prMsgs = make([]string, n)
		if r := eval(g, fakeTargets{}); r.Skipped != "failed: fetch" {
			t.Errorf("walk limit with %d commits: want failed: fetch, got %q", n, r.Skipped)
		}
	}

	// The attack: the PR adds a mapping in its first commit and changes the
	// file in its last. With the history unreadable the receipt must not say
	// "would apply" against the PR's own mapping.
	g = rebased()
	delete(g.commits, "r1")
	g.files = map[string]string{
		"r1:m.json":  manifest(entry("agent:"+agentUUID, "a.md")),
		"pre:m.json": manifest(),
		"mrg:m.json": manifest(entry("agent:"+agentUUID, "a.md")),
		"r1:a.md":    "old",
		"mrg:a.md":   "new",
	}
	if r := eval(g, fakeTargets{"agent:" + agentUUID: "old"}); r.Skipped != "failed: fetch" {
		t.Errorf("attack: want failed: fetch, got skipped=%q targets=%v", r.Skipped, r.Targets)
	}
}

func TestPreMergeBoundaryProvenNotRebase(t *testing.T) {
	// Squash of 2 commits onto readable history: the messages do not match, so
	// this is not a rebase and the single parent is the boundary.
	g := base()
	g.commits = map[string]Commit{"mrg": {Parents: []string{"pre"}, Message: "t (#7)"}, "pre": {Parents: []string{"p0"}, Message: "x"}}
	g.prMsgs = []string{"a", "b"}
	if r := eval(g, fakeTargets{}); r.PreMergeSHA != "pre" {
		t.Errorf("squash: got %q (%q)", r.PreMergeSHA, r.Skipped)
	}
	// A merge commit inside the walk cannot be a rebase either.
	g = base()
	g.commits = map[string]Commit{"mrg": {Parents: []string{"pre"}, Message: "t (#7)"}, "pre": {Parents: []string{"p0", "p1"}, Message: "merge"}}
	g.prMsgs = []string{"a", "b"}
	if r := eval(g, fakeTargets{}); r.PreMergeSHA != "pre" {
		t.Errorf("merge in walk: got %q (%q)", r.PreMergeSHA, r.Skipped)
	}
	// Fewer commits exist than the PR had: not a rebase.
	g = base()
	g.commits = map[string]Commit{"mrg": {Message: "root"}}
	g.prMsgs = []string{"a", "b"}
	if _, err := preMergeCommit(context.Background(), g, 7, "mrg"); err == nil {
		t.Error("a merge commit with no parent must stay an error")
	}
}
