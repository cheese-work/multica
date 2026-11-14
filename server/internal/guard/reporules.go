package guard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// RepoRulesFile is the one file a repository may use to state its own PR rules.
// The guard reads it as schema-checked data from the current tip of the PR's
// protected base branch, never from the PR branch or the working tree, and
// never runs it. It has the lowest precedence: any policy layer overrides it
// key by key, and a locked workspace or organization rule always wins.
const RepoRulesFile = ".github/agent-rules.yml"

const (
	maxRulesBytes = 64 << 10
	maxPattern    = 512
	maxTokens     = 32
	maxTokenLen   = 128
	maxReviewers  = 32
)

// TitleRules constrain a PR title. The merge-method tag is not part of it.
type TitleRules struct {
	Pattern string   `json:"pattern,omitempty" yaml:"pattern"`
	Require []string `json:"require,omitempty" yaml:"require"`
	Forbid  []string `json:"forbid,omitempty" yaml:"forbid"`
}

// Rules are the PR rules a policy layer or a repository file can state.
type Rules struct {
	Title TitleRules `json:"title,omitempty" yaml:"title"`
	// Trailer is a key such as "Refs:"; the PR body needs a line that starts with it.
	Trailer string `json:"trailer,omitempty" yaml:"trailer"`
}

// repoFile is the schema of RepoRulesFile.
type repoFile struct {
	Version   int        `yaml:"version"`
	Title     TitleRules `yaml:"title"`
	Trailer   string     `yaml:"trailer"`
	Reviewers []string   `yaml:"reviewers"`
}

var (
	trailerRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*:$`)
	loginRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9/_-]*$`)
	tagRe     = regexp.MustCompile(` \[(merge|squash)\]$`)
)

func (r *Rules) validate() error {
	if r == nil {
		return nil
	}
	t := r.Title
	if len(t.Pattern) > maxPattern {
		return fmt.Errorf("title.pattern is longer than %d characters", maxPattern)
	}
	if _, err := regexp.Compile(t.Pattern); err != nil {
		return fmt.Errorf("title.pattern: %w", err)
	}
	if len(t.Require)+len(t.Forbid) > maxTokens {
		return fmt.Errorf("title.require and title.forbid hold more than %d tokens", maxTokens)
	}
	for _, list := range [][]string{t.Require, t.Forbid} {
		for _, tok := range list {
			if tok == "" || len(tok) > maxTokenLen || strings.ContainsAny(tok, "\r\n") {
				return fmt.Errorf("title tokens must be 1 to %d characters on one line", maxTokenLen)
			}
		}
	}
	for _, tok := range t.Require {
		if contains(t.Forbid, tok) {
			return fmt.Errorf("title token %q is both required and forbidden", tok)
		}
	}
	if r.Trailer != "" && !trailerRe.MatchString(r.Trailer) {
		return fmt.Errorf("trailer %q must look like \"Refs:\"", r.Trailer)
	}
	return nil
}

// ParseRepoRules decodes and validates a rules file. Unknown keys, a second
// document and an oversized file are errors. It returns the file's rules and
// the reviewers it asks for.
func ParseRepoRules(raw []byte) (*Rules, []string, error) {
	if len(raw) > maxRulesBytes {
		return nil, nil, fmt.Errorf("file is larger than %d KiB", maxRulesBytes>>10)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var f repoFile
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil, errors.New("file is empty; it needs \"version: 1\"")
		}
		return nil, nil, fmt.Errorf("schema: %s", oneLine(err.Error(), 200))
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("schema: the file holds more than one YAML document")
	}
	if f.Version != 1 {
		return nil, nil, fmt.Errorf("schema: version must be 1, got %d", f.Version)
	}
	r := &Rules{Title: f.Title, Trailer: f.Trailer}
	if err := r.validate(); err != nil {
		return nil, nil, fmt.Errorf("schema: %w", err)
	}
	if len(f.Reviewers) > maxReviewers {
		return nil, nil, fmt.Errorf("schema: more than %d reviewers", maxReviewers)
	}
	for _, rv := range f.Reviewers {
		if !loginRe.MatchString(rv) || len(rv) > maxTokenLen {
			return nil, nil, fmt.Errorf("schema: reviewer %q is not a GitHub login or team", rv)
		}
	}
	return r, f.Reviewers, nil
}

// mergeRules folds layer rules into the resolved rules, key by key. A key is the
// pattern, the trailer, or one title token; set skips a frozen key.
func mergeRules(l Layer, set func(key string, locked bool, apply func()), acc *rulesAcc) {
	if l.Rules == nil {
		return
	}
	t := l.Rules.Title
	if t.Pattern != "" {
		set("rules:pattern", l.Locked, func() { acc.pattern = t.Pattern })
	}
	if l.Rules.Trailer != "" {
		set("rules:trailer", l.Locked, func() { acc.trailer = l.Rules.Trailer })
	}
	for _, tok := range t.Require {
		set("rules:token:"+tok, l.Locked, func() { acc.tokens[tok] = true })
	}
	for _, tok := range t.Forbid {
		set("rules:token:"+tok, l.Locked, func() { acc.tokens[tok] = false })
	}
}

// rulesAcc collects resolved rules while the ladder is walked.
type rulesAcc struct {
	pattern, trailer string
	tokens           map[string]bool // token -> required (true) or forbidden (false)
}

func (a *rulesAcc) rules() *Rules {
	r := &Rules{Trailer: a.trailer}
	r.Title.Pattern = a.pattern
	for tok, req := range a.tokens {
		if req {
			r.Title.Require = append(r.Title.Require, tok)
		} else {
			r.Title.Forbid = append(r.Title.Forbid, tok)
		}
	}
	sort.Strings(r.Title.Require)
	sort.Strings(r.Title.Forbid)
	if r.Trailer == "" && r.Title.Pattern == "" && len(a.tokens) == 0 {
		return nil
	}
	return r
}

// problems lists how a PR breaks the rules.
func (r *Rules) problems(title, body string) []string {
	out := r.titleProblems(title)
	if r != nil && r.Trailer != "" && !hasTrailer(body, r.Trailer) {
		out = append(out, "the body needs a "+r.Trailer+" line")
	}
	return out
}

// titleProblems checks the title without its merge-method tag.
func (r *Rules) titleProblems(title string) []string {
	if r == nil {
		return nil
	}
	title = tagRe.ReplaceAllString(title, "")
	var out []string
	if r.Title.Pattern != "" {
		if ok, err := regexp.MatchString(r.Title.Pattern, title); err == nil && !ok {
			out = append(out, "the title does not match "+r.Title.Pattern)
		}
	}
	for _, tok := range r.Title.Require {
		if !strings.Contains(title, tok) {
			out = append(out, "the title needs "+tok)
		}
	}
	for _, tok := range r.Title.Forbid {
		if strings.Contains(title, tok) {
			out = append(out, "the title must not contain "+tok)
		}
	}
	return out
}

func hasTrailer(body, key string) bool {
	for _, l := range strings.Split(body, "\n") {
		l = strings.TrimSpace(l)
		if len(l) > len(key) && strings.EqualFold(l[:len(key)], key) {
			return true
		}
	}
	return false
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// rulesError tells a bad file (fail closed, fix the file) from a failed read
// (fail closed, retry).
type rulesError struct {
	bad bool
	msg string
}

func (r *rulesError) Error() string { return r.msg }

func isBadRules(err error) bool {
	var re *rulesError
	return errors.As(err, &re) && re.bad
}

// fetchRepoRules reads RepoRulesFile at the current tip of base in repo: the
// PR's target repository, not a fork. It records the tip in e.rulesNote. A
// missing file and an unprotected base give no rules. A malformed file or a
// failed read is an error.
func (e *Engine) fetchRepoRules(ctx context.Context, repo, base string) (*Rules, []string, error) {
	segs := strings.Split(base, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	var br struct {
		Protected bool `json:"protected"`
		Commit    struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := e.ghJSON(ctx, &br, "api", "repos/"+repo+"/branches/"+strings.Join(segs, "/")); err != nil || br.Commit.SHA == "" {
		if err == nil {
			err = errors.New("no commit")
		}
		return nil, nil, &rulesError{msg: fmt.Sprintf("could not read the %s tip of %s (%s)", base, repo, firstLine(err.Error(), 120))}
	}
	sha := br.Commit.SHA
	if !br.Protected {
		e.rulesNote = fmt.Sprintf("repo rules ignored: %s of %s is not protected (base tip %s)", base, repo, sha)
		return nil, nil, nil
	}
	res, err := e.Runner.Run(ctx, "gh", "api", "-H", "Accept: application/vnd.github.raw+json",
		"repos/"+repo+"/contents/"+RepoRulesFile+"?ref="+sha)
	switch {
	case err != nil:
		return nil, nil, &rulesError{msg: fmt.Sprintf("could not read %s (%s)", RepoRulesFile, firstLine(err.Error(), 120))}
	case res.Exit != 0 && (strings.Contains(res.Stderr, "404") || strings.Contains(res.Stderr, "Not Found")):
		e.rulesNote = fmt.Sprintf("repo rules: no %s at base tip %s of %s", RepoRulesFile, sha, repo)
		return nil, nil, nil
	case res.Exit != 0:
		return nil, nil, &rulesError{msg: fmt.Sprintf("could not read %s at %s (%s)", RepoRulesFile, sha[:min(len(sha), 12)], firstLine(res.Stderr, 120))}
	}
	rules, revs, perr := ParseRepoRules([]byte(res.Stdout))
	if perr != nil {
		return nil, nil, &rulesError{bad: true, msg: fmt.Sprintf("%s at base tip %s of %s is malformed: %s", RepoRulesFile, sha[:min(len(sha), 12)], repo, perr)}
	}
	e.rulesNote = fmt.Sprintf("repo rules: %s at base tip %s of %s", RepoRulesFile, sha, repo)
	return rules, revs, nil
}

// effective is the policy for a call that targets base of repo: the ladder
// resolved for the call, with the repository file as the lowest layer. It
// reads the file again on every call, so a new base tip or a changed review
// policy applies at the next create, ready or check.
func (e *Engine) effective(ctx context.Context, repo, base string) (*Policy, error) {
	rules, revs, err := e.fetchRepoRules(ctx, repo, base)
	if err != nil {
		return nil, err
	}
	return e.Policy.resolve(Target{Repo: repo, Path: e.path}, rules, revs), nil
}

// rulesRefusal turns an error from effective into a refusal.
func rulesRefusal(err error, what string) (decision, bool) {
	if isBadRules(err) {
		return refuse(2, "%s refused: %s; fix the file on the base branch", what, err), true
	}
	return decision{}, false
}

// CheckPR reads one PR back and checks it against the rules without changing
// anything, for a CI check. sel is what `gh pr view` takes. It returns 1 when
// the PR breaks a rule.
func (e *Engine) CheckPR(ctx context.Context, sel []string) (int, error) {
	if e.Policy.lockdown {
		fmt.Fprintf(e.Err, "guard: refused: %s\n", lockdownMessage)
		return 2, nil
	}
	info, err := e.view(ctx, sel...)
	if err != nil {
		return 0, err
	}
	pl := e.match("gh", []string{"pr"})
	if pl == nil {
		pl = &Plugin{Name: "gh-pr", Builtin: "gh-pr"}
	}
	e.noRepair = true
	d := e.ensure(ctx, pl, info.URL, false, 1, nil)
	if d.kind == KindRefused {
		fmt.Fprintln(e.Err, "guard: "+d.line)
		return d.code, nil
	}
	fmt.Fprintln(e.Out, "guard: ok "+info.URL)
	return 0, nil
}
