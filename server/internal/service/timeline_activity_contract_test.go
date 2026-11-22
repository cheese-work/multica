package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Timeline lists on the web that must agree with Go (CHE-6603 batch K9, the
// CHE-6242 shape): a wakeup activity key the web does not know shows as the raw
// key, and a PR wake rule outside the web list reads with a generic sentence.
// contractProblem (web_client_lists_contract_test.go) states the rules.
//
// Limit of a web ⊆ Go check: it finds web values the server never writes, but
// it cannot prove that every new Go value has a web consumer.
const (
	wakeupActivityFile = "../../../packages/views/issues/components/wakeup-activity.tsx"
	coalesceListsFile  = "../../../packages/views/issues/components/timeline-coalesce.ts"
)

var (
	// Go activity keys the web timeline does not render yet: value -> the web
	// issue that adds them. CHE-9070 adds the CHE-6968 deferral keys.
	wakeupActivityServerOnly = map[string]string{
		"wake_deferred":      "CHE-9070",
		"wake_would_defer":   "CHE-9070",
		"wake_watch_missing": "CHE-9070",
	}
	prRulesServerOnly = map[string]string{}
)

// Coalescing is on purpose for these wakeup keys; the test pins the choice.
// A skipped or re-armed row repeats with the same text while a rule keeps
// firing, so a run of them folds into one row with a count. NEVER_COALESCE
// holds only rows whose details differ on every occurrence.
var wakeupCoalescesOnPurpose = map[string]string{
	"wakeup_skipped": "repeated skips by one rule fold into one row within 2 minutes",
	"wakeup_rearmed": "one provider outage re-arms the same rule repeatedly; one folded row is enough",
}

var actionName = regexp.MustCompile(`^[a-z]+(_[a-z]+)+$`)

func goConstsAt(t *testing.T, path string) map[string]string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		if spec, ok := n.(*ast.ValueSpec); ok {
			for i, name := range spec.Names {
				if lit, ok := valueAt(spec.Values, i).(*ast.BasicLit); ok && lit.Kind == token.STRING {
					out[name.Name], _ = strconv.Unquote(lit.Value)
				}
			}
		}
		return true
	})
	return out
}

// wakeupActivityKeys is every wakeupActivity* const of the service package.
func wakeupActivityKeys(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for name, v := range goConstsAt(t, "issue_wakeup_activity.go") {
		if strings.HasPrefix(name, "wakeupActivity") {
			out[v] = true
		}
	}
	if len(out) == 0 {
		t.Fatal("found no wakeupActivity* consts in issue_wakeup_activity.go")
	}
	return out
}

// prWakeRules is every SystemRule* const that prWakeupSetting accepts. The
// function itself decides, so a new PR rule joins the set when its case lands.
func prWakeRules(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for name, v := range goConstsAt(t, "issue_wakeup_system.go") {
		if strings.HasPrefix(name, "SystemRule") {
			if _, ok := prWakeupSetting(v); ok {
				out[v] = true
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("prWakeupSetting accepts none of the SystemRule* consts")
	}
	return out
}

// timelineActionWriters is every action key Go writes to the issue timeline
// from the three writers the coalesce lists name.
func timelineActionWriters(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for v := range wakeupActivityKeys(t) {
		out[v] = true
	}
	add := func(e ast.Expr) {
		if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, _ := strconv.Unquote(lit.Value); actionName.MatchString(s) {
				out[s] = true
			}
		}
	}
	read := func(path string, calls ...string) {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.KeyValueExpr:
				if id, ok := n.Key.(*ast.Ident); ok && id.Name == "Action" {
					add(n.Value)
				}
			case *ast.CallExpr:
				if id, ok := n.Fun.(*ast.Ident); ok {
					for _, c := range calls {
						if id.Name == c {
							for _, a := range n.Args {
								add(a)
							}
						}
					}
				}
			}
			return true
		})
	}
	read("../../cmd/server/activity_listeners.go", "record", "handleTaskActivity")
	read("../handler/squad.go")
	return out
}

func tsFileList(t *testing.T, path, name string) map[string]bool {
	t.Helper()
	return tsStringList(t, path, name)
}

func TestWebTimelineListsMatchGo(t *testing.T) {
	writers := timelineActionWriters(t)
	for _, c := range []struct {
		list    string
		rule    listRule
		client  map[string]bool
		server  map[string]bool
		srvOnly map[string]string
	}{
		{"WAKEUP_ACTIVITY_ACTIONS", equal, tsFileList(t, wakeupActivityFile, "WAKEUP_ACTIVITY_ACTIONS"), wakeupActivityKeys(t), wakeupActivityServerOnly},
		{"PR_RULES", equal, tsFileList(t, wakeupActivityFile, "PR_RULES"), prWakeRules(t), prRulesServerOnly},
		{"NEVER_COALESCE_ACTIONS", subsetOf, tsFileList(t, coalesceListsFile, "NEVER_COALESCE_ACTIONS"), writers, nil},
		{"NO_TIME_LIMIT_ACTIONS", subsetOf, tsFileList(t, coalesceListsFile, "NO_TIME_LIMIT_ACTIONS"), writers, nil},
	} {
		t.Run(c.list, func(t *testing.T) {
			if msg := contractProblem(c.list, c.rule, c.client, c.server, c.srvOnly, nil); msg != "" {
				t.Error(msg)
			}
		})
	}
}

// A rule that prWakeupSetting refuses is not a PR wake rule: pr_merge_owed is
// a write-ahead intent with no wake, so the web PR list must not hold it.
func TestPRWakeRulesControl(t *testing.T) {
	if _, ok := prWakeupSetting(SystemRulePRMergeOwed); ok {
		t.Fatal("prWakeupSetting accepts pr_merge_owed: the control no longer holds, update PR_RULES with its copy")
	}
	if tsFileList(t, wakeupActivityFile, "PR_RULES")[SystemRulePRMergeOwed] {
		t.Fatal("web PR_RULES holds pr_merge_owed, which prWakeupSetting refuses: remove it from the web list")
	}
	if !prWakeRules(t)[SystemRulePRMerged] {
		t.Fatal("prWakeRules lost pr_merged: the reader no longer finds the SystemRule* consts")
	}
}

func TestWakeupCoalescingChoicesStayOnPurpose(t *testing.T) {
	never := tsFileList(t, coalesceListsFile, "NEVER_COALESCE_ACTIONS")
	keys := wakeupActivityKeys(t)
	for _, v := range timelineKeys(wakeupCoalescesOnPurpose) {
		if !keys[v] {
			t.Errorf("%q is not a Go wakeup activity key: delete it from wakeupCoalescesOnPurpose", v)
		}
		if never[v] {
			t.Errorf("%q is in web NEVER_COALESCE_ACTIONS, but %s: remove it from the web list or from wakeupCoalescesOnPurpose", v, wakeupCoalescesOnPurpose[v])
		}
	}
}

// One drifted value per list fails with the list, the value and the fix.
func TestWebTimelineDriftIsNamed(t *testing.T) {
	for _, c := range []struct {
		list, file string
		rule       listRule
		server     map[string]bool
		srvOnly    map[string]string
	}{
		{"WAKEUP_ACTIVITY_ACTIONS", wakeupActivityFile, equal, wakeupActivityKeys(t), wakeupActivityServerOnly},
		{"PR_RULES", wakeupActivityFile, equal, prWakeRules(t), prRulesServerOnly},
		{"NEVER_COALESCE_ACTIONS", coalesceListsFile, subsetOf, timelineActionWriters(t), nil},
		{"NO_TIME_LIMIT_ACTIONS", coalesceListsFile, subsetOf, timelineActionWriters(t), nil},
	} {
		t.Run(c.list, func(t *testing.T) {
			real := tsFileList(t, c.file, c.list)
			added := map[string]bool{"drift_added_value": true}
			for k := range real {
				added[k] = true
			}
			assertNamed(t, contractProblem(c.list, c.rule, added, c.server, c.srvOnly, nil), c.list, "drift_added_value", "remove it from the web list")
			if c.rule == equal {
				keys := make([]string, 0, len(real))
				for k := range real {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				removed := map[string]bool{}
				for _, k := range keys[1:] {
					removed[k] = true
				}
				assertNamed(t, contractProblem(c.list, c.rule, removed, c.server, c.srvOnly, nil), c.list, keys[0], "add")
			}
		})
	}
}

func assertNamed(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("drift message %q lacks %q", got, want)
		}
	}
}

func timelineKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
