package service

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Web client lists that must agree with a Go vocabulary (CHE-6603 batch K3).
// A client list that drifts silently is the CHE-6242 shape: the UI misreads a
// value the server emits. Limit of a TS-subset check: it finds client values the
// server does not emit, but it cannot prove every new Go value has a client consumer.

const (
	realtimeSpecificEventsFile = "../../../packages/core/realtime/specific-events.ts"
	searchIndexEventsFile      = "../../../packages/core/search-index/hooks.ts"
	runStatusFile              = "../../../packages/core/issues/run-status.ts"
	protocolEventsFile         = "../../pkg/protocol/events.go"
)

// Declared exceptions. A stale entry fails the test.
var (
	// server-only: a Go value planned to land first -> the web issue (CHE-<n>) that adds it.
	activeTaskServerOnly = map[string]string{}
	preRunServerOnly     = map[string]string{}
	// client-only: a historical client value the server no longer emits -> reason.
	specificEventsClientOnly    = map[string]string{}
	searchIndexEventsClientOnly = map[string]string{}
)

type listRule int

const (
	equal    listRule = iota // client == server
	subsetOf                 // client ⊆ server
)

// contractProblem returns "" when the client list obeys the rule, else a message
// that names the list, the values and the fix.
func contractProblem(list string, rule listRule, client, server map[string]bool, serverOnly, clientOnly map[string]string) string {
	var out []string
	if missing := diffKeys(server, client); rule == equal {
		for _, v := range missing {
			if _, ok := serverOnly[v]; !ok {
				out = append(out, fmt.Sprintf("add %q to the web list, or declare it server-only with the web issue that adds it", v))
			}
		}
	}
	for _, v := range diffKeys(client, server) {
		if _, ok := clientOnly[v]; !ok {
			out = append(out, fmt.Sprintf("web value %q is not in the Go vocabulary: remove it from the web list, or add it to Go", v))
		}
	}
	for v := range serverOnly {
		if client[v] || !server[v] {
			out = append(out, fmt.Sprintf("stale server-only declaration %q: delete it", v))
		}
	}
	for v := range clientOnly {
		if server[v] || !client[v] {
			out = append(out, fmt.Sprintf("stale client-only declaration %q: delete it", v))
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return ""
	}
	return list + " drifted from Go: " + strings.Join(out, "; ")
}

func TestWebClientRealtimeEventListsAreGoEvents(t *testing.T) {
	events := goEventValues(t)
	for _, c := range []struct {
		list, file, name string
		clientOnly       map[string]string
	}{
		{"specificEvents", realtimeSpecificEventsFile, "specificEvents", specificEventsClientOnly},
		{"SEARCH_INDEX_EVENTS", searchIndexEventsFile, "SEARCH_INDEX_EVENTS", searchIndexEventsClientOnly},
	} {
		if msg := contractProblem(c.list, subsetOf, tsStringList(t, c.file, c.name), events, nil, c.clientOnly); msg != "" {
			t.Error(msg)
		}
	}
}

func TestWebClientRunStatusListsMatchRunGate(t *testing.T) {
	inFlight := map[string]bool{}
	for status, on := range runGateInFlight {
		if on {
			inFlight[status] = true
		}
	}
	preRun := map[string]bool{}
	for status := range inFlight {
		if status != "running" {
			preRun[status] = true
		}
	}
	if msg := contractProblem("ACTIVE_TASK", equal, tsStringList(t, runStatusFile, "ACTIVE_TASK"), inFlight, activeTaskServerOnly, nil); msg != "" {
		t.Error(msg)
	}
	if msg := contractProblem("PRE_RUN_STATUSES", equal, tsStringList(t, runStatusFile, "PRE_RUN_STATUSES"), preRun, preRunServerOnly, nil); msg != "" {
		t.Error(msg)
	}
}

// Each rule reports one drifted value with the list, the value and the fix.
func TestWebClientListContractNamesTheDrift(t *testing.T) {
	set := func(v ...string) map[string]bool {
		m := map[string]bool{}
		for _, s := range v {
			m[s] = true
		}
		return m
	}
	for _, c := range []struct {
		name         string
		rule         listRule
		client, goes map[string]bool
		serverOnly   map[string]string
		clientOnly   map[string]string
		want         string
	}{
		{"equal: client lacks a Go value", equal, set("a"), set("a", "b"), nil, nil, `LIST drifted from Go: add "b" to the web list`},
		{"equal: client has an extra value", equal, set("a", "x"), set("a"), nil, nil, `web value "x" is not in the Go vocabulary`},
		{"subset: client has an unsupported value", subsetOf, set("a", "x"), set("a"), nil, nil, `web value "x" is not in the Go vocabulary`},
		{"stale server-only", equal, set("a", "b"), set("a", "b"), map[string]string{"b": "CHE-1"}, nil, `stale server-only declaration "b"`},
		{"stale client-only", subsetOf, set("a"), set("a", "x"), nil, map[string]string{"x": "old"}, `stale client-only declaration "x"`},
	} {
		got := contractProblem("LIST", c.rule, c.client, c.goes, c.serverOnly, c.clientOnly)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want it to contain %q", c.name, got, c.want)
		}
	}
	// A subset rule does not demand coverage; a declared server-only value is allowed.
	if got := contractProblem("LIST", subsetOf, set("a"), set("a", "b"), nil, nil); got != "" {
		t.Errorf("subset rule must not require coverage, got %q", got)
	}
	if got := contractProblem("LIST", equal, set("a"), set("a", "b"), map[string]string{"b": "CHE-1"}, nil); got != "" {
		t.Errorf("declared server-only value must pass, got %q", got)
	}
}

// goEventValues is the set of string values of the Event* consts in protocol/events.go.
func goEventValues(t *testing.T) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), protocolEventsFile, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		if spec, ok := n.(*ast.ValueSpec); ok {
			for i, name := range spec.Names {
				if lit, ok := valueAt(spec.Values, i).(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.HasPrefix(name.Name, "Event") {
					s, _ := strconv.Unquote(lit.Value)
					out[s] = true
				}
			}
		}
		return true
	})
	if len(out) == 0 {
		t.Fatal("found no Event* consts in protocol/events.go")
	}
	return out
}

func valueAt(values []ast.Expr, i int) ast.Expr {
	if i < len(values) {
		return values[i]
	}
	return nil
}

func diffKeys(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// tsStringList reads the double-quoted strings of the first array literal after
// `name` (an array, or `new Set([...])`) in a TS file, skipping comments.
func tsStringList(t *testing.T, file, name string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	src := stripTSComments(string(raw))
	at := strings.Index(src, name+" =")
	if at < 0 {
		at = strings.Index(src, name+":")
	}
	if at < 0 {
		t.Fatalf("%s not found in %s", name, file)
	}
	eq := at + strings.Index(src[at:], "= ") + 2
	opener := "["
	if strings.HasPrefix(src[eq:], "new Set") {
		opener = "([" // skip a type argument such as Set<AgentTask["status"]>
	}
	open := eq + strings.Index(src[eq:], opener) + len(opener) - 1
	list := map[string]bool{}
	for i := open + 1; i < len(src) && src[i] != ']'; i++ {
		if src[i] == '"' {
			end := strings.IndexByte(src[i+1:], '"')
			list[src[i+1:i+1+end]] = true
			i += end + 1
		}
	}
	if len(list) == 0 {
		t.Fatalf("%s in %s has no string values", name, file)
	}
	return list
}

// stripTSComments drops // and /* */ comments and leaves string literals intact.
func stripTSComments(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '"' || s[i] == '\'' || s[i] == '`':
			q := s[i]
			j := i + 1
			for j < len(s) && s[j] != q {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			b.WriteString(s[i:min(j+1, len(s))])
			i = j
		case strings.HasPrefix(s[i:], "//"):
			for i < len(s) && s[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case strings.HasPrefix(s[i:], "/*"):
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			i += end + 3
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
