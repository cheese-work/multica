package ghsnapshot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func configSyncServer(t *testing.T, routes map[string]string, posted *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_t", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
			return
		}
		if r.Method == http.MethodPost {
			b := new(strings.Builder)
			_, _ = fmt.Fprint(b, r.URL.Path+" ")
			var in struct{ Body string }
			var raw map[string]string
			_ = json.NewDecoder(r.Body).Decode(&raw)
			in.Body = raw["body"]
			*posted = b.String() + in.Body
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		body, ok := routes[r.URL.RequestURI()]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
}

func TestConfigSyncReaderMapsAPIFields(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("héllo\r\n"))
	var posted string
	srv := configSyncServer(t, map[string]string{
		"/repos/o/r/pulls/7": `{"merged":true,"merged_by":{"login":"Owner","type":"User"},"base":{"ref":"main"},"merge_commit_sha":"mrg"}`,
		"/repos/o/r/issues/7/timeline?per_page=100&page=1": `[{"event":"commented"},{"event":"auto_merge_enabled"}]`,
		"/repos/o/r/pulls/7/commits?per_page=100&page=1":   `[{"commit":{"message":"a"}},{"commit":{"message":"b"}}]`,
		"/repos/o/r/commits/mrg":                           `{"parents":[{"sha":"p1"},{"sha":"p2"}],"commit":{"message":"m"}}`,
		"/repos/o/r/contents/dir/a%20b.md?ref=mrg":         `{"type":"file","encoding":"base64","content":"` + enc + `"}`,
		"/repos/o/r/contents/dir?ref=mrg":                  `[{"type":"file"}]`,
	}, &posted)
	defer srv.Close()
	rd := NewConfigSyncReader(newTestClient(t, srv.URL), 1, "o", "r")
	ctx := context.Background()

	pr, err := rd.PullRequest(ctx, 7)
	if err != nil || !pr.Merged || pr.MergedByLogin != "Owner" || pr.MergedByType != "User" || pr.BaseRef != "main" || pr.MergeCommitSHA != "mrg" {
		t.Fatalf("PullRequest = %+v, %v", pr, err)
	}
	if ev, err := rd.TimelineEvents(ctx, 7); err != nil || strings.Join(ev, ",") != "commented,auto_merge_enabled" {
		t.Fatalf("TimelineEvents = %v, %v", ev, err)
	}
	if m, err := rd.PRCommitMessages(ctx, 7); err != nil || strings.Join(m, ",") != "a,b" {
		t.Fatalf("PRCommitMessages = %v, %v", m, err)
	}
	if c, err := rd.Commit(ctx, "mrg"); err != nil || len(c.Parents) != 2 || c.Parents[0] != "p1" || c.Message != "m" {
		t.Fatalf("Commit = %+v, %v", c, err)
	}
	if b, found, err := rd.File(ctx, "mrg", "dir/a b.md"); err != nil || !found || string(b) != "héllo\r\n" {
		t.Fatalf("File = %q %v %v", b, found, err)
	}
	if _, found, err := rd.File(ctx, "mrg", "missing.md"); err != nil || found {
		t.Fatalf("missing file: found=%v err=%v", found, err)
	}
	if _, _, err := rd.File(ctx, "mrg", "dir"); err == nil {
		t.Fatal("a directory must be an error, not file content")
	}
	if err := rd.PostComment(ctx, 7, "hello"); err != nil || posted != "/repos/o/r/issues/7/comments hello" {
		t.Fatalf("PostComment = %q, %v", posted, err)
	}
}

func TestConfigSyncReaderFailsClosedOnShortPagesLimit(t *testing.T) {
	// 100 timeline events on every page: more pages than the cap must error,
	// never silently truncate (a missed auto_merge_enabled would pass the check).
	page := "[" + strings.TrimSuffix(strings.Repeat(`{"event":"commented"},`, 100), ",") + "]"
	routes := map[string]string{}
	for i := 1; i <= 20; i++ {
		routes[fmt.Sprintf("/repos/o/r/issues/7/timeline?per_page=100&page=%d", i)] = page
	}
	var posted string
	srv := configSyncServer(t, routes, &posted)
	defer srv.Close()
	if _, err := NewConfigSyncReader(newTestClient(t, srv.URL), 1, "o", "r").TimelineEvents(context.Background(), 7); err == nil {
		t.Fatal("expected an error when the timeline exceeds the page cap")
	}
}

// GitHub's PR-commits endpoint returns at most 250 commits and gives no sign
// that it truncated. A list that reaches the cap cannot prove the PR's real
// commit list, so the reader must error and never return it as complete.
func TestConfigSyncReaderPRCommitsFailsClosedAtAPICap(t *testing.T) {
	commit := func(n int) string {
		return "[" + strings.TrimSuffix(strings.Repeat(`{"commit":{"message":"m"}},`, n), ",") + "]"
	}
	for _, tc := range []struct {
		name    string
		pages   []int
		wantErr bool
	}{
		{"under the cap", []int{100, 100, 49}, false},
		{"at the cap", []int{100, 100, 50}, true},
	} {
		routes := map[string]string{}
		for i, n := range tc.pages {
			routes[fmt.Sprintf("/repos/o/r/pulls/7/commits?per_page=100&page=%d", i+1)] = commit(n)
		}
		var posted string
		srv := configSyncServer(t, routes, &posted)
		msgs, err := NewConfigSyncReader(newTestClient(t, srv.URL), 1, "o", "r").PRCommitMessages(context.Background(), 7)
		srv.Close()
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err=%v wantErr=%v (got %d messages)", tc.name, err, tc.wantErr, len(msgs))
		}
	}
}
