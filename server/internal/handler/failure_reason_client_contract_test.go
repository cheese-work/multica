package handler

import (
	"go/ast"
	"go/token"
	"regexp"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// A failure_reason the clients cannot label renders as the generic "Failed"
// (mobile) or the raw wire value (web) (CHE-6603 batch K8). The lists below
// join the web and mobile tables to the Go taxonomy. A coverage check finds a Go
// reason a client lacks; a subset check finds a client value Go does not know.
// Neither can prove that a new reason has the right copy, which the web and
// mobile locale tests own.
const (
	webFailureFile    = "packages/views/agents/components/tabs/task-failure.ts"
	mobileFailureFile = "apps/mobile/lib/failure-reason-label.ts"
)

// goFailureReasons is every value of the canonical taskfailure taxonomy.
func goFailureReasons() map[string]bool {
	out := map[string]bool{}
	for _, r := range taskfailure.AllReasons() {
		out[r.String()] = true
	}
	return out
}

// goCancellationReasons is the set of case labels of taskErrorType that return
// "cancelled" (server/internal/service/task.go). The daemon writes
// "cancelled" (server/internal/daemon/daemon.go) and the service still reads
// the legacy "user_cancelled".
func goCancellationReasons(t *testing.T) map[string]bool {
	t.Helper()
	file := parseGoFile(t, "../service/task.go")
	consts := goFileConsts(file)
	out := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "taskErrorType" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if !ok || len(clause.Body) != 1 {
				return true
			}
			ret, ok := clause.Body[0].(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 1 {
				return true
			}
			if lit, ok := ret.Results[0].(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.Trim(lit.Value, `"`) == "cancelled" {
				for _, e := range clause.List {
					out[goStr(t, e, consts)] = true
				}
			}
			return true
		})
		return false
	})
	if len(out) == 0 {
		t.Fatal("found no cancellation reasons in taskErrorType")
	}
	return out
}

type failureReasonPair struct {
	listContract
	web func(t *testing.T, src string) map[string]bool
	gov func(t *testing.T) map[string]bool
}

func failureReasonPairs() []failureReasonPair {
	allReasons := func(*testing.T) map[string]bool { return goFailureReasons() }
	return []failureReasonPair{
		{
			listContract{
				list: "FAILURE_REASON_I18N_KEYS", tsFile: webFailureFile,
				goList: "taskfailure.AllReasons (server/pkg/taskfailure/failure.go)", rule: contractCover,
			},
			func(t *testing.T, src string) map[string]bool {
				return tsObjectKeys(t, src, "FAILURE_REASON_I18N_KEYS")
			},
			allReasons,
		},
		{
			listContract{
				list: "REASONS", tsFile: mobileFailureFile,
				goList: "taskfailure.AllReasons (server/pkg/taskfailure/failure.go)", rule: contractCover,
			},
			func(t *testing.T, src string) map[string]bool { return tsLiteralList(t, src, "REASONS") },
			allReasons,
		},
		{
			listContract{
				list: "ACTION_REQUIRED_FAILURE_REASONS", tsFile: webFailureFile,
				goList: "taskfailure.AllReasons (server/pkg/taskfailure/failure.go)", rule: contractSubset,
			},
			func(t *testing.T, src string) map[string]bool {
				return tsLiteralList(t, src, "ACTION_REQUIRED_FAILURE_REASONS")
			},
			allReasons,
		},
		{
			listContract{
				list: "CANCELLATION_FAILURE_REASONS", tsFile: webFailureFile,
				goList: "taskErrorType cancellation cases (server/internal/service/task.go)", rule: contractEqual,
				clientOnly: map[string]string{"manual": "pre-MUL-1949 value on historical rows; no writer emits it"},
			},
			func(t *testing.T, src string) map[string]bool {
				return tsLiteralList(t, src, "CANCELLATION_FAILURE_REASONS")
			},
			goCancellationReasons,
		},
	}
}

func TestFailureReasonListsMatchGoTaxonomy(t *testing.T) {
	for _, p := range failureReasonPairs() {
		t.Run(p.list+"@"+p.tsFile, func(t *testing.T) {
			ts := p.web(t, readRepoFile(t, p.tsFile))
			if problems := p.problems(ts, p.gov(t)); len(problems) > 0 {
				t.Fatal(strings.Join(problems, "\n"))
			}
		})
	}
}

// driftEdit edits the first token `value` (a quoted list item or an object key)
// after the declaration of `list`. The client files hold several lists that share
// values, so a plain text replace would hit the wrong one.
func driftEdit(t *testing.T, src, list, value, replace string) string {
	t.Helper()
	decl := regexp.MustCompile(`\b(?:const|let|var)\s+` + regexp.QuoteMeta(list) + `\b`).FindStringIndex(src)
	if decl == nil {
		t.Fatalf("declaration of %s not found", list)
	}
	token := regexp.MustCompile(`([\s"'\[{,])` + regexp.QuoteMeta(value) + `(["':,])`)
	tail := src[decl[0]:]
	loc := token.FindStringSubmatchIndex(tail)
	if loc == nil {
		t.Fatalf("%s: value %q not found after its declaration", list, value)
	}
	return src[:decl[0]] + tail[:loc[2]] + tail[loc[2]:loc[3]] + replace + tail[loc[4]:loc[5]] + tail[loc[1]:]
}

// One reason added to or removed from the real client text must fail with a
// message that names the list, the value and the fix.
func TestFailureReasonListDriftIsCaught(t *testing.T) {
	for _, p := range failureReasonPairs() {
		t.Run(p.list+"@"+p.tsFile, func(t *testing.T) {
			src := readRepoFile(t, p.tsFile)
			gov := p.gov(t)
			var goBacked string
			for _, v := range contractKeys(p.web(t, src)) {
				if gov[v] {
					goBacked = v
					break
				}
			}
			if p.rule != contractCover {
				// A value only the client has fails with "remove".
				added := p.web(t, driftEdit(t, src, p.list, goBacked, goBacked+`", "drift_added_reason`))
				assertProblem(t, p.problems(added, gov), p.list, "drift_added_reason", "remove")
			}
			if p.rule != contractSubset {
				// A Go value the client lacks fails with "add".
				removed := p.web(t, driftEdit(t, src, p.list, goBacked, "drift_other_reason"))
				assertProblem(t, p.problems(removed, gov), p.list, goBacked, "add")
			}
		})
	}
}

func TestCoverContractRule(t *testing.T) {
	c := listContract{list: "L", tsFile: "f.ts", goList: "g", rule: contractCover}
	set := func(v ...string) map[string]bool {
		m := map[string]bool{}
		for _, x := range v {
			m[x] = true
		}
		return m
	}
	if p := c.problems(set("a", "extra"), set("a")); len(p) > 0 {
		t.Fatalf("a cover contract tolerates a client-only value: %v", p)
	}
	assertProblem(t, c.problems(set("a"), set("a", "b")), "L", "b", "add")
	c.serverOnly = map[string]string{"b": "CHE-1"}
	if p := c.problems(set("a"), set("a", "b")); len(p) > 0 {
		t.Fatalf("a server-only declaration absorbs a Go value: %v", p)
	}
}
