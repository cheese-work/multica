package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

const (
	relTestIssueID  = "33333333-3333-4333-8333-333333333333"
	relTestMemberID = "44444444-4444-4444-8444-444444444444"
	relTestAgentID  = "55555555-5555-4555-8555-555555555555"
)

// relTestThreads is the issue's root list as the server holds it. The server
// stub below filters it by relation= the way handler.ListComments does.
func relTestThreads() []map[string]any {
	root := func(id string, withMe, waiting bool) map[string]any {
		return map[string]any{
			"id": id, "author_type": "agent", "author_id": relTestAgentID,
			"type": "comment", "content": "root " + id,
			"created_at": "2026-11-20T02:00:00Z", "updated_at": "2026-11-20T02:00:00Z",
			"thread_relation": map[string]any{"with_me": withMe, "agents_only": !withMe, "waiting_on_me": waiting},
		}
	}
	return []map[string]any{
		root("c-waiting-1", true, true),
		root("c-answered", true, false),
		root("c-agents-only", false, false),
		root("c-waiting-2", true, true),
	}
}

type relTestServer struct {
	*httptest.Server
	query      url.Values
	relUnavail bool
}

func newRelTestServer(t *testing.T, threads []map[string]any) *relTestServer {
	t.Helper()
	rs := &relTestServer{}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/issues/"+relTestIssueID:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": relTestIssueID, "identifier": "TST-3"})
		case r.URL.Path == "/api/workspaces/ws-1/members":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"user_id": relTestMemberID, "name": "Cheese", "email": "cheese@example.com"}})
		case r.URL.Path == "/api/issues/"+relTestIssueID+"/comments":
			rs.query = r.URL.Query()
			out := threads
			if rs.relUnavail {
				w.Header().Set("X-Multica-Thread-Relation", "unavailable")
			} else {
				out = nil
				for _, c := range threads {
					rel, _ := c["thread_relation"].(map[string]any)
					keep := true
					switch rs.query.Get("relation") {
					case "with_me":
						keep = rel["with_me"] == true
					case "without_me":
						keep = rel["with_me"] != true
					case "waiting":
						keep = rel["waiting_on_me"] == true
					}
					if keep {
						out = append(out, c)
					}
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(rs.Close)
	setCLITestServerEnv(t, rs.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	return rs
}

func runRelList(t *testing.T, set map[string]string) ([]byte, error) {
	t.Helper()
	cmd := newIssueCommentListTestCmd()
	for k, v := range set {
		if err := cmd.Flags().Set(k, v); err != nil {
			t.Fatalf("set --%s: %v", k, err)
		}
	}
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	drain := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(r); drain <- b }()
	runErr := runIssueCommentList(cmd, []string{relTestIssueID})
	w.Close()
	os.Stdout = orig
	return <-drain, runErr
}

func idsOf(t *testing.T, out []byte) []string {
	t.Helper()
	var got []map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, out)
	}
	ids := make([]string, 0, len(got))
	for _, c := range got {
		ids = append(ids, c["id"].(string))
	}
	return ids
}

// --waiting-on <member> prints only the roots that wait on that member.
func TestRunIssueCommentList_WaitingOnPrintsOnlyWaitingRoots(t *testing.T) {
	srv := newRelTestServer(t, relTestThreads())
	out, err := runRelList(t, map[string]string{"waiting-on": relTestMemberID, "roots-only": "true", "summary": "true"})
	if err != nil {
		t.Fatalf("runIssueCommentList: %v", err)
	}
	if got := strings.Join(idsOf(t, out), ","); got != "c-waiting-1,c-waiting-2" {
		t.Errorf("printed roots = %q, want only the waiting roots", got)
	}
	for k, want := range map[string]string{"relation": "waiting", "for": relTestMemberID, "roots_only": "true", "summary": "true"} {
		if got := srv.query.Get(k); got != want {
			t.Errorf("query %s = %q, want %q", k, got, want)
		}
	}
}

// --waiting-on takes a member name too and resolves it to the member id.
func TestRunIssueCommentList_WaitingOnResolvesMemberName(t *testing.T) {
	srv := newRelTestServer(t, relTestThreads())
	if _, err := runRelList(t, map[string]string{"waiting-on": "Cheese"}); err != nil {
		t.Fatalf("runIssueCommentList: %v", err)
	}
	if got := srv.query.Get("for"); got != relTestMemberID {
		t.Errorf("for = %q, want the member id", got)
	}
}

func TestRunIssueCommentList_RelationFlagsMapToQuery(t *testing.T) {
	cases := []struct {
		name         string
		set          map[string]string
		wantRelation string
		wantFor      string
		wantIDs      string
	}{
		{"with-me", map[string]string{"with-me": "true"}, "with_me", "", "c-waiting-1,c-answered,c-waiting-2"},
		{"without-me", map[string]string{"without-me": "true"}, "without_me", "", "c-agents-only"},
		{"with-me for agent", map[string]string{"with-me": "true", "for": relTestAgentID}, "with_me", relTestAgentID, "c-waiting-1,c-answered,c-waiting-2"},
		{"for alone asks for the relation without a filter", map[string]string{"for": relTestAgentID}, "1", relTestAgentID, "c-waiting-1,c-answered,c-agents-only,c-waiting-2"},
		{"with-me with thread", map[string]string{"with-me": "true", "thread": "c-waiting-1"}, "with_me", "", "c-waiting-1,c-answered,c-waiting-2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newRelTestServer(t, relTestThreads())
			out, err := runRelList(t, tc.set)
			if err != nil {
				t.Fatalf("runIssueCommentList: %v", err)
			}
			if got := srv.query.Get("relation"); got != tc.wantRelation {
				t.Errorf("relation = %q, want %q", got, tc.wantRelation)
			}
			if got := srv.query.Get("for"); got != tc.wantFor {
				t.Errorf("for = %q, want %q", got, tc.wantFor)
			}
			if got := strings.Join(idsOf(t, out), ","); got != tc.wantIDs {
				t.Errorf("printed = %q, want %q", got, tc.wantIDs)
			}
		})
	}
}

// Without a relation flag the request carries neither relation nor for.
func TestRunIssueCommentList_NoRelationFlagSendsNoRelationParams(t *testing.T) {
	srv := newRelTestServer(t, relTestThreads())
	if _, err := runRelList(t, nil); err != nil {
		t.Fatalf("runIssueCommentList: %v", err)
	}
	for _, k := range []string{"relation", "for"} {
		if _, ok := srv.query[k]; ok {
			t.Errorf("query carries %q without a relation flag", k)
		}
	}
}

func TestRunIssueCommentList_RelationFlagConflicts(t *testing.T) {
	cases := []struct {
		name string
		set  map[string]string
		want string
	}{
		{"with and without", map[string]string{"with-me": "true", "without-me": "true"}, "mutually exclusive"},
		{"with and waiting", map[string]string{"with-me": "true", "waiting-on": relTestMemberID}, "mutually exclusive"},
		{"without and waiting", map[string]string{"without-me": "true", "waiting-on": relTestMemberID}, "mutually exclusive"},
		{"waiting-on and a different for", map[string]string{"waiting-on": relTestMemberID, "for": relTestAgentID}, "--waiting-on"},
		{"empty waiting-on", map[string]string{"waiting-on": " "}, "--waiting-on"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newRelTestServer(t, relTestThreads())
			_, err := runRelList(t, tc.set)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// A filtered read must never print an unfiltered list as if it were filtered.
func TestRunIssueCommentList_RelationUnavailableIsAnError(t *testing.T) {
	srv := newRelTestServer(t, relTestThreads())
	srv.relUnavail = true
	_, err := runRelList(t, map[string]string{"waiting-on": relTestMemberID})
	if err == nil || !strings.Contains(err.Error(), "thread relation") {
		t.Fatalf("err = %v, want a thread relation unavailable error", err)
	}
	// Without a relation flag the header is not our business.
	if _, err := runRelList(t, nil); err != nil {
		t.Fatalf("no flag: %v", err)
	}
}

// --compact and --summary carry thread_relation only when a relation flag is
// set, so an existing agent read does not grow (plan CHE-6385 section 8).
func TestRunIssueCommentList_CompactByteIdenticalWithoutRelationFlag(t *testing.T) {
	withRelation := relTestThreads()
	withoutRelation := relTestThreads()
	for _, c := range withoutRelation {
		delete(c, "thread_relation")
	}
	for _, mode := range []string{"compact", "summary"} {
		t.Run(mode, func(t *testing.T) {
			newRelTestServer(t, withRelation)
			got, err := runRelList(t, map[string]string{mode: "true"})
			if err != nil {
				t.Fatalf("runIssueCommentList: %v", err)
			}
			newRelTestServer(t, withoutRelation)
			want, err := runRelList(t, map[string]string{mode: "true"})
			if err != nil {
				t.Fatalf("runIssueCommentList: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s output changed without a relation flag:\n got: %s\nwant: %s", mode, got, want)
			}
			if bytes.Contains(got, []byte("thread_relation")) {
				t.Errorf("%s output carries thread_relation without a relation flag", mode)
			}
		})
	}
}

func TestRunIssueCommentList_CompactKeepsRelationWithFlag(t *testing.T) {
	newRelTestServer(t, relTestThreads())
	for _, flag := range []map[string]string{
		{"compact": "true", "with-me": "true"},
		{"summary": "true", "without-me": "true"},
		{"compact": "true", "for": relTestAgentID},
		{"compact": "true", "waiting-on": relTestMemberID},
	} {
		out, err := runRelList(t, flag)
		if err != nil {
			t.Fatalf("%v: %v", flag, err)
		}
		if len(out) > 3 && !bytes.Contains(out, []byte("thread_relation")) {
			t.Errorf("%v: output lost thread_relation: %s", flag, out)
		}
	}
}
