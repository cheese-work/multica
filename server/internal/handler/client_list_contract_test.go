package handler

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A web list that must match a Go vocabulary, with no test that joins the two,
// is the CHE-6242 shape: the server adds a value, the client does not know it,
// and a card or control breaks. Each listContract below joins one pair
// (CHE-6603 batch K1; later batches reuse the readers in this file).
//
// contractEqual: both sides hold the same values. contractSubset: every web
// value must exist in Go (the client picks part of a vocabulary). A subset
// check finds unsupported client values; it cannot prove that every new Go
// value has a client consumer. contractCover: every Go value must exist in the
// client list (a lookup the client must be able to render); the client may hold
// more, such as values Go writes from outside the checked list.
type contractRule int

const (
	contractEqual contractRule = iota
	contractSubset
	contractCover
)

type listContract struct {
	list   string // the web list, as named in its file
	tsFile string // repo-relative path of the web file
	goList string // what the Go side is, for the failure message
	rule   contractRule
	// serverOnly: Go values the web list does not carry yet, value -> the web
	// issue (CHE-<n>) that adds them. clientOnly: web values Go no longer emits,
	// value -> reason. A declaration that stops being true fails the test.
	serverOnly map[string]string
	clientOnly map[string]string
}

// problems returns one message per broken part of the contract.
func (c listContract) problems(ts, goVals map[string]bool) []string {
	var out []string
	fix := func(values []string, why string) {
		if len(values) > 0 {
			out = append(out, fmt.Sprintf("%s (%s) vs %s: %s %v", c.list, c.tsFile, c.goList, why, values))
		}
	}
	var undeclaredServer, undeclaredClient, staleServer, staleClient, badIssue []string
	for _, v := range diffKeys(goVals, ts) {
		if _, ok := c.serverOnly[v]; !ok && c.rule != contractSubset {
			undeclaredServer = append(undeclaredServer, v)
		}
	}
	for _, v := range diffKeys(ts, goVals) {
		if _, ok := c.clientOnly[v]; !ok && c.rule != contractCover {
			undeclaredClient = append(undeclaredClient, v)
		}
	}
	for _, v := range contractKeys(c.serverOnly) {
		if !goVals[v] || ts[v] {
			staleServer = append(staleServer, v)
		}
		if !webIssueKey.MatchString(c.serverOnly[v]) {
			badIssue = append(badIssue, v)
		}
	}
	for _, v := range contractKeys(c.clientOnly) {
		if !ts[v] || goVals[v] || c.clientOnly[v] == "" {
			staleClient = append(staleClient, v)
		}
	}
	fix(undeclaredServer, "Go emits values the client list lacks; add them to the client list (with their rendering and copy), or declare them server-only with the web issue that adds them:")
	fix(undeclaredClient, "the client list has values Go does not have; remove them from the client list, or declare a historical client-only value with a reason:")
	fix(staleServer, "stale server-only declaration (value is not in Go, or the client list now has it); delete it:")
	fix(staleClient, "stale client-only declaration (value is not in the client list, Go now has it, or it has no reason); delete it:")
	fix(badIssue, "server-only declaration without a CHE-<n> issue:")
	return out
}

func contractKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- Go readers ----

// goFileConsts maps every string const in the file to its value.
func goFileConsts(file *ast.File) map[string]string {
	consts := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		if spec, ok := n.(*ast.ValueSpec); ok {
			for i, name := range spec.Names {
				if lit, ok := valueAt(spec.Values, i).(*ast.BasicLit); ok && lit.Kind == token.STRING {
					consts[name.Name], _ = strconv.Unquote(lit.Value)
				}
			}
		}
		return true
	})
	return consts
}

func parseGoFile(t *testing.T, path string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

// goStr resolves a string literal or a const of the same file.
func goStr(t *testing.T, e ast.Expr, consts map[string]string) string {
	t.Helper()
	switch e := e.(type) {
	case *ast.BasicLit:
		s, _ := strconv.Unquote(e.Value)
		return s
	case *ast.Ident:
		if v, ok := consts[e.Name]; ok {
			return v
		}
	}
	t.Fatalf("cannot resolve %T to a string constant", e)
	return ""
}

// goStringSlice reads the package-level `var name = []string{...}` of a file.
func goStringSlice(t *testing.T, path, name string) map[string]bool {
	t.Helper()
	file := parseGoFile(t, path)
	consts := goFileConsts(file)
	out := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, id := range spec.Names {
			if lit, ok := valueAt(spec.Values, i).(*ast.CompositeLit); ok && id.Name == name {
				for _, el := range lit.Elts {
					out[goStr(t, el, consts)] = true
				}
			}
		}
		return true
	})
	if len(out) == 0 {
		t.Fatalf("found no values for %s in %s", name, path)
	}
	return out
}

// goMapKeys reads the keys of the package-level `var name = map[K]V{...}`.
func goMapKeys(t *testing.T, path, name string) map[string]bool {
	t.Helper()
	file := parseGoFile(t, path)
	consts := goFileConsts(file)
	out := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, id := range spec.Names {
			if lit, ok := valueAt(spec.Values, i).(*ast.CompositeLit); ok && id.Name == name {
				for _, el := range lit.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						out[goStr(t, kv.Key, consts)] = true
					}
				}
			}
		}
		return true
	})
	if len(out) == 0 {
		t.Fatalf("found no keys for %s in %s", name, path)
	}
	return out
}

// goConstsWithPrefix reads the string consts of a file whose name starts with
// prefix, as values (e.g. RunBudgetOK = "ok").
func goConstsWithPrefix(t *testing.T, path, prefix string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for name, v := range goFileConsts(parseGoFile(t, path)) {
		if strings.HasPrefix(name, prefix) {
			out[v] = true
		}
	}
	if len(out) == 0 {
		t.Fatalf("found no %s* string consts in %s", prefix, path)
	}
	return out
}

// ---- TypeScript literal reader ----

// stripTSComments removes // and /* */ comments and leaves string literals alone.
func stripTSComments(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '"' || c == '\'' || c == '`':
			j := i + 1
			for j < len(src) && src[j] != c {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(src) {
				j = len(src) - 1
			}
			b.WriteString(src[i : j+1])
			i = j
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			i += end + 3
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// tsInitializer returns the text after `const|let|var name [: type] =`, comments
// stripped, wherever the declaration is (top level or inside a function body).
func tsInitializer(t *testing.T, src, name string) string {
	t.Helper()
	re := regexp.MustCompile(`\b(?:const|let|var)\s+` + regexp.QuoteMeta(name) + `\b[^=;]*=[^=>]`)
	clean := stripTSComments(src)
	loc := re.FindStringIndex(clean)
	if loc == nil {
		t.Fatalf("declaration of %s not found", name)
	}
	return clean[loc[1]-1:]
}

// tsBalanced returns the text between the opener at s[0] and its match.
func tsBalanced(t *testing.T, s string) string {
	t.Helper()
	depth := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\'', '`':
			for i++; i < len(s) && s[i] != c; i++ {
				if s[i] == '\\' {
					i++
				}
			}
		case '[', '{', '(':
			depth++
		case ']', '}', ')':
			depth--
			if depth == 0 {
				return s[1:i]
			}
		}
	}
	t.Fatal("unbalanced brackets in TypeScript initializer")
	return ""
}

var tsStringLit = regexp.MustCompile(`^\s*(?:"((?:[^"\\]|\\.)*)"|'((?:[^'\\]|\\.)*)')\s*$`)

func tsUnquote(el string) (string, bool) {
	m := tsStringLit.FindStringSubmatch(el)
	if m == nil {
		return "", false
	}
	if m[1] != "" || m[2] == "" {
		return m[1], true
	}
	return m[2], true
}

// tsLiteralList reads `name = [..]`, `name = [..] as const` or
// `name = new Set([..])` (optionally with a type argument) from a TS source.
func tsLiteralList(t *testing.T, src, name string) map[string]bool {
	t.Helper()
	init := strings.TrimSpace(tsInitializer(t, src, name))
	if strings.HasPrefix(init, "new Set") {
		i := strings.Index(init, "(")
		if i < 0 {
			t.Fatalf("%s: new Set without arguments", name)
		}
		init = strings.TrimSpace(init[i+1:])
	}
	if !strings.HasPrefix(init, "[") {
		t.Fatalf("%s is not an array literal", name)
	}
	body := tsBalanced(t, init)
	out := map[string]bool{}
	for _, el := range splitTopLevel(t, body) {
		if strings.TrimSpace(el) == "" {
			continue
		}
		v, ok := tsUnquote(el)
		if !ok {
			t.Fatalf("%s: element %q is not a plain string literal", name, strings.TrimSpace(el))
		}
		out[v] = true
	}
	if len(out) == 0 {
		t.Fatalf("%s holds no values", name)
	}
	return out
}

// splitTopLevel splits an array or object body at its depth-0 commas.
func splitTopLevel(t *testing.T, body string) []string {
	t.Helper()
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(body); i++ {
		switch c := body[i]; c {
		case '"', '\'', '`':
			for i++; i < len(body) && body[i] != c; i++ {
				if body[i] == '\\' {
					i++
				}
			}
		case '[', '{', '(':
			depth++
		case ']', '}', ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, body[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, body[start:])
}

var tsObjectKey = regexp.MustCompile(`^\s*(?:([A-Za-z_$][\w$]*)|"([^"]*)"|'([^']*)')\s*(?::|$)`)

// tsObjectKeys reads the top-level keys of `name = { .. }` (also with a type
// annotation such as Record<..>).
func tsObjectKeys(t *testing.T, src, name string) map[string]bool {
	t.Helper()
	init := strings.TrimSpace(tsInitializer(t, src, name))
	if !strings.HasPrefix(init, "{") {
		t.Fatalf("%s is not an object literal", name)
	}
	body := tsBalanced(t, init)
	out := map[string]bool{}
	for _, entry := range splitTopLevel(t, body) {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		m := tsObjectKey.FindStringSubmatch(entry)
		if m == nil {
			t.Fatalf("%s: cannot read the key of entry %q", name, strings.TrimSpace(entry))
		}
		out[m[1]+m[2]+m[3]] = true
	}
	if len(out) == 0 {
		t.Fatalf("%s holds no keys", name)
	}
	return out
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	src, err := os.ReadFile("../../../" + rel)
	if err != nil {
		t.Fatal(err)
	}
	return string(src)
}

// ---- the K1 pairs ----

type k1Pair struct {
	listContract
	web func(t *testing.T, src string) map[string]bool
	gov func(t *testing.T) map[string]bool
}

func k1Pairs() []k1Pair {
	array := func(name string) func(*testing.T, string) map[string]bool {
		return func(t *testing.T, src string) map[string]bool { return tsLiteralList(t, src, name) }
	}
	return []k1Pair{
		{
			listContract{
				list: "CONFIG_CHANGE_FIELDS", tsFile: "packages/core/issues/decision-action-preview.ts",
				goList: "decisionaction.configFields (server/internal/decisionaction/config.go)", rule: contractEqual,
			},
			array("CONFIG_CHANGE_FIELDS"),
			func(t *testing.T) map[string]bool {
				return goStringSlice(t, "../decisionaction/config.go", "configFields")
			},
		},
		{
			listContract{
				list: "RUN_BUDGET_STATES", tsFile: "packages/core/run-budget/schema.ts",
				goList: "the RunBudget* consts (server/internal/service/run_budget_snapshot.go)", rule: contractEqual,
			},
			array("RUN_BUDGET_STATES"),
			func(t *testing.T) map[string]bool {
				return goConstsWithPrefix(t, "../service/run_budget_snapshot.go", "RunBudget")
			},
		},
		{
			listContract{
				list: "GOVERNED_FIELDS", tsFile: "packages/views/settings/components/delegated-writes-section.tsx",
				goList: "delegatedWriteFields (server/internal/handler/workspace_delegated_writes.go)", rule: contractEqual,
			},
			array("GOVERNED_FIELDS"),
			func(t *testing.T) map[string]bool {
				return goStringSlice(t, "workspace_delegated_writes.go", "delegatedWriteFields")
			},
		},
		{
			listContract{
				list: "JOBS", tsFile: "packages/views/settings/components/background-agents-tab.tsx",
				goList: "keys of bgagents.Catalog (server/internal/bgagents/bgagents.go)", rule: contractSubset,
				// The TS comment states it: compact joins when its personal override ships.
				serverOnly: map[string]string{"compact": "CHE-4121"},
			},
			array("JOBS"),
			func(t *testing.T) map[string]bool { return goMapKeys(t, "../bgagents/bgagents.go", "Catalog") },
		},
	}
}

// The web lists in the pairs above must keep matching their Go vocabulary.
func TestWebClientListsMatchGoVocabularies(t *testing.T) {
	for _, p := range k1Pairs() {
		t.Run(p.list, func(t *testing.T) {
			ts := p.web(t, readRepoFile(t, p.tsFile))
			if problems := p.problems(ts, p.gov(t)); len(problems) > 0 {
				t.Fatal(strings.Join(problems, "\n"))
			}
		})
	}
}

// One value added to or removed from the real web file must fail with a
// message that names the list, the value and the fix. The fixture edits the
// real TS text, so the reader and the rule are both exercised.
func TestWebClientListDriftIsCaught(t *testing.T) {
	for _, p := range k1Pairs() {
		t.Run(p.list, func(t *testing.T) {
			src := readRepoFile(t, p.tsFile)
			gov := p.gov(t)
			real := p.web(t, src)
			first := contractKeys(real)[0]

			// A value only the web has: fails with "remove".
			added := p.web(t, strings.Replace(src, "\""+first+"\"", "\""+first+"\", \"drift_added_value\"", 1))
			assertProblem(t, p.problems(added, gov), p.list, "drift_added_value", "remove")

			// A Go value the web lacks (equality only): fails with "add".
			if p.rule == contractEqual {
				removed := p.web(t, strings.Replace(src, "\""+first+"\"", "\"drift_other_value\"", 1))
				assertProblem(t, p.problems(removed, gov), p.list, first, "add")
			}
		})
	}
}

func assertProblem(t *testing.T, problems []string, list, value, fix string) {
	t.Helper()
	joined := strings.Join(problems, "\n")
	for _, want := range []string{list, value, fix} {
		if !strings.Contains(joined, want) {
			t.Fatalf("drifted fixture was not caught with %q in the message; got %q", want, joined)
		}
	}
}

func TestListContractDeclarations(t *testing.T) {
	c := listContract{list: "L", tsFile: "f.ts", goList: "g", rule: contractEqual}
	set := func(v ...string) map[string]bool {
		m := map[string]bool{}
		for _, x := range v {
			m[x] = true
		}
		return m
	}
	t.Run("server-only declaration absorbs a Go value", func(t *testing.T) {
		c := c
		c.serverOnly = map[string]string{"b": "CHE-1"}
		if p := c.problems(set("a"), set("a", "b")); len(p) > 0 {
			t.Fatalf("unexpected problems: %v", p)
		}
	})
	t.Run("server-only declaration goes stale when the web list adds the value", func(t *testing.T) {
		c := c
		c.serverOnly = map[string]string{"b": "CHE-1"}
		assertProblem(t, c.problems(set("a", "b"), set("a", "b")), "L", "b", "stale server-only")
	})
	t.Run("server-only declaration goes stale when Go drops the value", func(t *testing.T) {
		c := c
		c.serverOnly = map[string]string{"b": "CHE-1"}
		assertProblem(t, c.problems(set("a"), set("a")), "L", "b", "stale server-only")
	})
	t.Run("server-only declaration needs a web issue", func(t *testing.T) {
		c := c
		c.serverOnly = map[string]string{"b": "later"}
		assertProblem(t, c.problems(set("a"), set("a", "b")), "L", "b", "CHE-<n>")
	})
	t.Run("client-only declaration absorbs a historical web value", func(t *testing.T) {
		c := c
		c.clientOnly = map[string]string{"old": "removed from the API in 2025"}
		if p := c.problems(set("a", "old"), set("a")); len(p) > 0 {
			t.Fatalf("unexpected problems: %v", p)
		}
	})
	t.Run("a Go-emitted value is never a client-only exception", func(t *testing.T) {
		c := c
		c.clientOnly = map[string]string{"a": "pretend"}
		assertProblem(t, c.problems(set("a"), set("a")), "L", "a", "stale client-only")
	})
	t.Run("client-only declaration needs a reason", func(t *testing.T) {
		c := c
		c.clientOnly = map[string]string{"old": ""}
		assertProblem(t, c.problems(set("old"), set()), "L", "old", "reason")
	})
	t.Run("an undeclared Go-only value fails an equality contract", func(t *testing.T) {
		assertProblem(t, c.problems(set("a"), set("a", "b")), "L", "b", "add")
	})
	t.Run("a subset contract tolerates a Go-only value", func(t *testing.T) {
		c := c
		c.rule = contractSubset
		if p := c.problems(set("a"), set("a", "b")); len(p) > 0 {
			t.Fatalf("unexpected problems: %v", p)
		}
	})
}

func TestTSLiteralReader(t *testing.T) {
	const src = `
// const FAKE = ["from a comment"];
export const PLAIN = ["a", 'b', "c//not a comment"] as const;
const TYPED: readonly string[] = [
  "x", /* inline */ "y",
];
export const SET = new Set(["s1", "s2"]);
const SET_T = new Set<string>([
  "t1",
]);
export const MAP: Record<string, number> = {
  alpha: 1,
  "beta-two": 2,
  'gamma': { nested: ["not", "keys"] },
  delta,
};
function f() {
  const inner = ["i1", "i2"];
  return inner;
}
`
	for _, tc := range []struct {
		name string
		got  map[string]bool
		want []string
	}{
		{"array as const", tsLiteralList(t, src, "PLAIN"), []string{"a", "b", "c//not a comment"}},
		{"typed array with comments", tsLiteralList(t, src, "TYPED"), []string{"x", "y"}},
		{"new Set", tsLiteralList(t, src, "SET"), []string{"s1", "s2"}},
		{"new Set with type argument", tsLiteralList(t, src, "SET_T"), []string{"t1"}},
		{"list in a function body", tsLiteralList(t, src, "inner"), []string{"i1", "i2"}},
		{"object keys", tsObjectKeys(t, src, "MAP"), []string{"alpha", "beta-two", "gamma", "delta"}},
	} {
		if got := contractKeys(tc.got); strings.Join(got, "|") != strings.Join(sortedStrings(tc.want), "|") {
			t.Errorf("%s: got %v, want %v", tc.name, got, sortedStrings(tc.want))
		}
	}
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
