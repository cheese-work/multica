package handler

import (
	"fmt"
	"go/ast"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The server accepts a notification preference group only if it is in
// validNotifGroups. The web client keeps its own copies of that list: the
// array that the optimistic patch walks, the type union, and the display lists
// of the settings screens. A group that exists only in Go is accepted by the
// API but has no toggle and no optimistic update. This test joins the lists
// (CHE-6604, the CHE-6242 shape).
//
// The limit of this check: it finds a Go key without a web consumer, and a web
// key that Go rejects. It cannot prove that a new key has copy or a toggle.
const (
	notifGroupsPatchTS  = "../../../packages/core/notification-preferences/patch.ts"
	notifGroupsTypesTS  = "../../../packages/core/types/notification-preference.ts"
	notifGroupsTabTSX   = "../../../packages/views/settings/components/notifications-tab.tsx"
	notifGroupsSearchTS = "../../../packages/views/settings/components/use-settings-search-index.ts"
	notifGroupsMobile   = "../../../apps/mobile/app/(app)/[workspace]/more/settings/notifications.tsx"
)

// notifGroupsServerOnly lists a Go key that is planned to land in the web array
// first: key → the web issue that adds it. Empty on a tree where every Go key
// has a web toggle. A declaration that no longer holds fails the test.
var notifGroupsServerOnly = map[string]string{
	"reactions": "CHE-6404",
	"autopilot": "CHE-6404",
}

type notifGroupInputs struct {
	server     []string            // Go validNotifGroups
	web        []string            // NOTIFICATION_GROUP_KEYS
	union      []string            // type NotificationGroupKey
	display    map[string][]string // display list name → keys; each must be ⊆ web
	serverOnly map[string]string
}

func TestNotificationGroupKeysMatchWebClient(t *testing.T) {
	in := notifGroupInputs{
		server: notifGroupsFromGo(t),
		web:    tsStringList(t, notifGroupsPatchTS, "NOTIFICATION_GROUP_KEYS"),
		union:  notifGroupsUnion(t),
		display: map[string][]string{
			"INBOX_GROUP_KEYS":    tsStringList(t, notifGroupsTabTSX, "INBOX_GROUP_KEYS"),
			"NOTIFICATION_GROUPS": tsStringList(t, notifGroupsSearchTS, "NOTIFICATION_GROUPS"),
			"mobile INBOX_GROUPS": notifGroupsFromMobile(t),
		},
		serverOnly: notifGroupsServerOnly,
	}
	if problems := notifGroupDrift(in); len(problems) > 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}
}

// Each rule must fail on its own drift, and a matching state must pass.
func TestNotificationGroupDriftIsReported(t *testing.T) {
	base := func() notifGroupInputs {
		return notifGroupInputs{
			server:     []string{"a", "b", "c"},
			web:        []string{"a", "b"},
			union:      []string{"a", "b"},
			display:    map[string][]string{"LIST": {"a"}},
			serverOnly: map[string]string{"c": "CHE-1"},
		}
	}
	if problems := notifGroupDrift(base()); len(problems) > 0 {
		t.Fatalf("matching inputs must pass: %v", problems)
	}
	cases := []struct {
		rule   string
		mutate func(*notifGroupInputs)
		want   string
	}{
		{"1 Go key not in web or declared", func(in *notifGroupInputs) { in.server = append(in.server, "d") }, `Go key(s) ["d"]`},
		{"1 web key not in Go", func(in *notifGroupInputs) { in.web = append(in.web, "z"); in.union = append(in.union, "z") }, `["z"] are in the web array but not in Go`},
		{"2 stale declaration", func(in *notifGroupInputs) { in.web = append(in.web, "c"); in.union = append(in.union, "c") }, `stale declaration "c"`},
		{"3 declared key not in Go", func(in *notifGroupInputs) { in.serverOnly["gone"] = "CHE-2" }, `declared key "gone" is not in Go`},
		{"4 union differs", func(in *notifGroupInputs) { in.union = []string{"a"} }, "NotificationGroupKey"},
		{"5 display key not in web", func(in *notifGroupInputs) { in.display["LIST"] = []string{"a", "q"} }, `LIST has ["q"]`},
		{"6 declaration without issue key", func(in *notifGroupInputs) { in.serverOnly["c"] = "later" }, "CHE-<n>"},
	}
	for _, tc := range cases {
		t.Run(tc.rule, func(t *testing.T) {
			in := base()
			tc.mutate(&in)
			got := strings.Join(notifGroupDrift(in), "\n")
			if !strings.Contains(got, tc.want) {
				t.Fatalf("message %q lacks %q", got, tc.want)
			}
		})
	}
}

// notifGroupDrift returns one message per problem; none means the lists agree.
// Each message names the fix.
func notifGroupDrift(in notifGroupInputs) []string {
	srv, web, decl := contractSet(in.server), contractSet(in.web), in.serverOnly
	var problems []string
	var add, remove []string
	for _, k := range in.server {
		if _, declared := decl[k]; !web[k] && !declared {
			add = append(add, k)
		}
	}
	for _, k := range in.web {
		if !srv[k] {
			remove = append(remove, k)
		}
	}
	sort.Strings(add)
	sort.Strings(remove)
	if len(add) > 0 {
		problems = append(problems, fmt.Sprintf("Go key(s) %q are not in the web NOTIFICATION_GROUP_KEYS: add them to the web array, the NotificationGroupKey union and the settings toggles, or declare each in notifGroupsServerOnly as key → the CHE-<n> issue that adds it", add))
	}
	if len(remove) > 0 {
		problems = append(problems, fmt.Sprintf("keys %q are in the web array but not in Go validNotifGroups: remove them from the web array; the API rejects them", remove))
	}
	for _, k := range contractSortedKeys(decl) {
		switch {
		case !srv[k]:
			problems = append(problems, fmt.Sprintf("declared key %q is not in Go validNotifGroups: delete it from notifGroupsServerOnly", k))
		case web[k]:
			problems = append(problems, fmt.Sprintf("stale declaration %q: the web array now has it; delete it from notifGroupsServerOnly", k))
		case !webIssueKey.MatchString(decl[k]):
			problems = append(problems, fmt.Sprintf("declaration %q needs the web issue key CHE-<n>, got %q", k, decl[k]))
		}
	}
	if a, b := slices.Sorted(slices.Values(in.union)), slices.Sorted(slices.Values(in.web)); !slices.Equal(a, b) {
		problems = append(problems, fmt.Sprintf("the type union NotificationGroupKey %q differs from NOTIFICATION_GROUP_KEYS %q: make them equal", a, b))
	}
	names := make([]string, 0, len(in.display))
	for name := range in.display {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var extra []string
		for _, k := range in.display[name] {
			if !web[k] {
				extra = append(extra, k)
			}
		}
		if len(extra) > 0 {
			problems = append(problems, fmt.Sprintf("display list %s has %q, which is not in NOTIFICATION_GROUP_KEYS: remove it, or add it to the web array", name, extra))
		}
	}
	return problems
}

// notifGroupsFromGo reads the keys of the validNotifGroups map literal.
func notifGroupsFromGo(t *testing.T) []string {
	t.Helper()
	_, vars := goPackageDecls(t, ".")
	lit, ok := vars["validNotifGroups"]
	if !ok {
		t.Fatal("validNotifGroups not found")
	}
	var out []string
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			t.Fatalf("cannot read element %T of validNotifGroups", el)
		}
		s, err := strconv.Unquote(kv.Key.(*ast.BasicLit).Value)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		t.Fatal("validNotifGroups is empty")
	}
	return out
}

// notifGroupsUnion reads the members of `type NotificationGroupKey = | "a" | "b";`.
func notifGroupsUnion(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile(notifGroupsTypesTS)
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`type NotificationGroupKey =((?:\s*\|?\s*"[^"]+")+);`).FindSubmatch(src)
	if block == nil {
		t.Fatal("type NotificationGroupKey not found in notification-preference.ts")
	}
	var out []string
	for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllSubmatch(block[1], -1) {
		out = append(out, string(m[1]))
	}
	return out
}

// notifGroupsFromMobile reads the `key: "..."` entries of the mobile INBOX_GROUPS list.
func notifGroupsFromMobile(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile(notifGroupsMobile)
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)\bINBOX_GROUPS: \{.*?\}\[\] = \[(.*?)\n\];`).FindSubmatch(src)
	if block == nil {
		t.Fatal("INBOX_GROUPS not found in the mobile notifications screen")
	}
	var out []string
	for _, m := range regexp.MustCompile(`\bkey: "([^"]+)"`).FindAllSubmatch(block[1], -1) {
		out = append(out, string(m[1]))
	}
	if len(out) == 0 {
		t.Fatal("found no keys in the mobile INBOX_GROUPS")
	}
	return out
}
