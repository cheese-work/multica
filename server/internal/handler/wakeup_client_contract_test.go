package handler

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/eventcontract"
)

// The wakeup editor and the timeline keep their own copies of four server
// vocabularies. A copy that falls behind fails quietly: the event picker drops a
// stored event, an issue loses its default rule row, the Gantt view draws no
// wakeup arrow, or the server rejects a "run ends" wakeup. Each list is joined
// to its Go source here (CHE-6644, the CHE-6242 shape).
//
// The limit of a client ⊆ server check: it finds client values the server does
// not know, but it cannot prove that a new Go value has a client consumer.
const (
	wakeupDraftTS    = "../../../packages/views/issues/components/wakeup-draft.ts"
	wakeupDefaultsTS = "../../../packages/views/issues/components/issue-wakeup-defaults.tsx"
	wakeupGraphTS    = "../../../packages/core/api/issue-graph-schema.ts"
)

// wakeupServerOnly lists a server value that is planned to land in the web list
// first: value → the web issue that adds it. wakeupClientOnly lists a historical
// web value the server no longer emits: value → why it stays. A Go-emitted value
// is never a client-only exception, and a declaration that no longer holds fails.
// Both are empty today.
var (
	wakeupServerOnly = map[string]map[string]string{}
	wakeupClientOnly = map[string]map[string]string{}
)

type wakeupListContract struct {
	name   string // the web list
	source string // where the Go values come from
	// subset checks web ⊆ server; otherwise the two sets must be equal.
	subset bool
	server func(t *testing.T) []string
	client func(t *testing.T) []string
}

func wakeupListContracts() []wakeupListContract {
	return []wakeupListContract{
		{
			name:   "WAKEUP_EVENT_TYPES",
			source: "eventcontract.WakeupTypes",
			server: func(*testing.T) []string { return eventcontract.WakeupTypes },
			client: func(t *testing.T) []string { return tsStringList(t, wakeupDraftTS, "WAKEUP_EVENT_TYPES") },
		},
		{
			name:   "RUN_END_EVENTS",
			source: "eventcontract.WakeupTypes",
			subset: true,
			server: func(*testing.T) []string { return eventcontract.WakeupTypes },
			client: func(t *testing.T) []string { return tsStringList(t, wakeupDraftTS, "RUN_END_EVENTS") },
		},
		{
			name:   "PLATFORM_RULES",
			source: "builtinWakeupRules without child_done",
			server: func(t *testing.T) []string {
				var rules []string
				for _, rule := range goStringList(t, "../service", "builtinWakeupRules") {
					if rule != "child_done" { // its own row on the issue wakeup panel
						rules = append(rules, rule)
					}
				}
				return rules
			},
			client: func(t *testing.T) []string { return tsStringList(t, wakeupDefaultsTS, "PLATFORM_RULES") },
		},
		{
			name:   "WAKEUP_CLASSES",
			source: "the wakeupClass* constants",
			server: func(t *testing.T) []string { return goStringConsts(t, ".", "wakeupClass") },
			client: func(t *testing.T) []string { return tsStringList(t, wakeupGraphTS, "WAKEUP_CLASSES") },
		},
	}
}

func TestWakeupClientListsMatchServer(t *testing.T) {
	for _, c := range wakeupListContracts() {
		t.Run(c.name, func(t *testing.T) {
			server, client := c.server(t), c.client(t)
			if problems := wakeupListDrift(c, server, client, wakeupServerOnly[c.name], wakeupClientOnly[c.name]); len(problems) > 0 {
				t.Fatal(strings.Join(problems, "\n"))
			}
		})
	}
}

// One value added to or removed from either side must fail, naming the list, the
// values and the fix.
func TestWakeupClientListDriftIsReported(t *testing.T) {
	for _, c := range wakeupListContracts() {
		t.Run(c.name, func(t *testing.T) {
			server, client := c.server(t), c.client(t)
			if len(server) == 0 || len(client) == 0 {
				t.Fatalf("%s: found no values (server %d, web %d)", c.name, len(server), len(client))
			}
			dropped := client[0]
			added := "drift.added_value"
			if !c.subset {
				got := strings.Join(wakeupListDrift(c, server, client[1:], nil, nil), "\n")
				for _, want := range []string{c.name, dropped, "add "} {
					if !strings.Contains(got, want) {
						t.Errorf("a value removed from the web list: message %q lacks %q", got, want)
					}
				}
			}
			got := strings.Join(wakeupListDrift(c, server, append(slices.Clone(client), added), nil, nil), "\n")
			for _, want := range []string{c.name, added, "remove "} {
				if !strings.Contains(got, want) {
					t.Errorf("a value added to the web list: message %q lacks %q", got, want)
				}
			}
			if problems := wakeupListDrift(c, server, client, nil, nil); len(problems) > 0 {
				t.Errorf("matching lists must pass: %v", problems)
			}
		})
	}
}

// Declarations must hold exactly: a server-only value that the web list now has,
// or that the server dropped, is stale; so is a client-only value the server now
// emits.
func TestWakeupClientListStaleDeclarationsFail(t *testing.T) {
	c := wakeupListContracts()[0] // WAKEUP_EVENT_TYPES, equality
	server, client := c.server(t), c.client(t)
	cases := []struct {
		label      string
		client     []string
		serverOnly map[string]string
		clientOnly map[string]string
		want       string
	}{
		{"server-only value is now in the web list", client, map[string]string{client[0]: "CHE-1"}, nil, "stale server-only"},
		{"server-only value is gone from the server", client, map[string]string{"gone.value": "CHE-1"}, nil, "stale server-only"},
		{"server-only needs a web issue key", client[1:], map[string]string{client[0]: "later"}, nil, "CHE-<n>"},
		{"client-only value is now emitted by the server", client, nil, map[string]string{client[0]: "old"}, "stale client-only"},
		{"client-only needs a reason", append(slices.Clone(client), "old.value"), nil, map[string]string{"old.value": ""}, "reason"},
		{"declared server-only value passes", client[1:], map[string]string{client[0]: "CHE-1"}, nil, ""},
		{"declared client-only value passes", append(slices.Clone(client), "old.value"), nil, map[string]string{"old.value": "kept for stored rules"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			got := strings.Join(wakeupListDrift(c, server, tc.client, tc.serverOnly, tc.clientOnly), "\n")
			if tc.want == "" && got != "" {
				t.Fatalf("expected no problems, got %q", got)
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("message %q lacks %q", got, tc.want)
			}
		})
	}
}

var webIssueKey = regexp.MustCompile(`^CHE-\d+$`)

// wakeupListDrift returns one message per problem; none means the pair holds.
func wakeupListDrift(c wakeupListContract, server, client []string, serverOnly, clientOnly map[string]string) []string {
	srv, cli := contractSet(server), contractSet(client)
	var problems []string
	var add, remove []string
	for v := range srv {
		if !cli[v] && !c.subset {
			if _, declared := serverOnly[v]; !declared {
				add = append(add, v)
			}
		}
	}
	for v := range cli {
		if !srv[v] {
			if _, declared := clientOnly[v]; !declared {
				remove = append(remove, v)
			}
		}
	}
	sort.Strings(add)
	sort.Strings(remove)
	if len(add) > 0 {
		problems = append(problems, fmt.Sprintf("web %s differs from %s: add %v to the web list, with its rendering and copy, or declare it in wakeupServerOnly as value → the CHE-<n> issue that adds it", c.name, c.source, add))
	}
	if len(remove) > 0 {
		problems = append(problems, fmt.Sprintf("web %s differs from %s: remove %v from the web list; the server does not emit it", c.name, c.source, remove))
	}
	for _, v := range contractSortedKeys(serverOnly) {
		switch {
		case c.subset || !srv[v] || cli[v]:
			problems = append(problems, fmt.Sprintf("stale server-only declaration %q for %s: delete it from wakeupServerOnly", v, c.name))
		case !webIssueKey.MatchString(serverOnly[v]):
			problems = append(problems, fmt.Sprintf("server-only declaration %q for %s needs the web issue key CHE-<n>, got %q", v, c.name, serverOnly[v]))
		}
	}
	for _, v := range contractSortedKeys(clientOnly) {
		switch {
		case srv[v] || !cli[v]:
			problems = append(problems, fmt.Sprintf("stale client-only declaration %q for %s: delete it from wakeupClientOnly", v, c.name))
		case strings.TrimSpace(clientOnly[v]) == "":
			problems = append(problems, fmt.Sprintf("client-only declaration %q for %s needs a reason", v, c.name))
		}
	}
	return problems
}

// tsStringList reads the string array `name` from a web source file.
func tsStringList(t *testing.T, path, name string) []string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)\b` + regexp.QuoteMeta(name) + `\b[^=\n]*= \[(.*?)\]`).FindSubmatch(src)
	if block == nil {
		t.Fatalf("%s not found in %s", name, filepath.Base(path))
	}
	var out []string
	for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllSubmatch(block[1], -1) {
		out = append(out, string(m[1]))
	}
	return out
}

// goStringList reads the `[]string{...}` variable `name` declared in the Go files
// of dir. An element is a string literal or a string constant of the package.
func goStringList(t *testing.T, dir, name string) []string {
	t.Helper()
	consts, vars := goPackageDecls(t, dir)
	list, ok := vars[name]
	if !ok {
		t.Fatalf("variable %s not found in %s", name, dir)
	}
	var out []string
	for _, expr := range list.Elts {
		switch e := expr.(type) {
		case *ast.BasicLit:
			s, _ := strconv.Unquote(e.Value)
			out = append(out, s)
		case *ast.Ident:
			v, ok := consts[e.Name]
			if !ok {
				t.Fatalf("cannot resolve %s in %s to a string constant", e.Name, name)
			}
			out = append(out, v)
		default:
			t.Fatalf("cannot read element %T of %s", expr, name)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s is empty", name)
	}
	return out
}

// goStringConsts returns the values of the string constants of dir whose name
// starts with prefix.
func goStringConsts(t *testing.T, dir, prefix string) []string {
	t.Helper()
	consts, _ := goPackageDecls(t, dir)
	var out []string
	for name, v := range consts {
		if strings.HasPrefix(name, prefix) {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatalf("no string constants named %s* in %s", prefix, dir)
	}
	return out
}

// goPackageDecls collects the package-level string constants and []string
// composite-literal variables of the non-test Go files in dir.
func goPackageDecls(t *testing.T, dir string) (map[string]string, map[string]*ast.CompositeLit) {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	consts := map[string]string{}
	vars := map[string]*ast.CompositeLit{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, id := range vs.Names {
					switch v := valueAt(vs.Values, i).(type) {
					case *ast.BasicLit:
						if gen.Tok == token.CONST && v.Kind == token.STRING {
							consts[id.Name], _ = strconv.Unquote(v.Value)
						}
					case *ast.CompositeLit:
						if gen.Tok == token.VAR {
							vars[id.Name] = v
						}
					}
				}
			}
		}
	}
	return consts, vars
}

func contractSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[v] = true
	}
	return set
}

func contractSortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
