package ghsnapshot

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func tokenOrElse(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_secret","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
			return
		}
		next(w, r)
	}
}

func TestListRunJobsReadsOnePageOfTheLatestAttempt(t *testing.T) {
	var got string
	srv := httptest.NewServer(tokenOrElse(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path + "?" + r.URL.RawQuery
		w.Header().Set("Link", `<https://api.github.com/x?page=2>; rel="next"`)
		_, _ = w.Write([]byte(`{"jobs":[
		 {"id":7,"name":"build","conclusion":"failure","completed_at":"2026-11-06T01:02:03Z","steps":[{"status":"in_progress","conclusion":null}]},
		 {"id":8,"name":"lint","conclusion":"success","completed_at":"2026-11-06T01:02:04Z","steps":[{"status":"completed","conclusion":"success"}]}]}`))
	}))
	defer srv.Close()
	jobs, more, _, err := newTestClient(t, srv.URL).ListRunJobs(context.Background(), 5, "o", "r", 99, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/repos/o/r/actions/runs/99/attempts/2/jobs?filter=latest&page=3&per_page=100"; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	if !more || len(jobs) != 2 || jobs[0].ID != 7 || jobs[0].Conclusion != "failure" || jobs[0].CompletedAt.IsZero() {
		t.Fatalf("more=%v jobs=%+v", more, jobs)
	}
	if !jobs[0].RunnerLost || jobs[1].RunnerLost {
		t.Fatalf("runner lost = %v, %v; want true, false", jobs[0].RunnerLost, jobs[1].RunnerLost)
	}
}

// The log answers 302 to a signed blob URL on another host. It must stream
// with no Range header, no Authorization on the blob host, and no 8 MB cap.
func TestJobLogStreamsThroughTheRedirectWithoutARangeHeader(t *testing.T) {
	var blobAuth, blobRange string
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blobAuth, blobRange = r.Header.Get("Authorization"), r.Header.Get("Range")
		_, _ = io.Copy(w, strings.NewReader(strings.Repeat("x", 9<<20)))
	}))
	defer blob.Close()
	var apiRange string
	api := httptest.NewServer(tokenOrElse(func(w http.ResponseWriter, r *http.Request) {
		apiRange = r.Header.Get("Range")
		if r.URL.Path != "/repos/o/r/actions/jobs/7/logs" {
			t.Errorf("path = %s", r.URL.Path)
		}
		http.Redirect(w, r, blob.URL+"/signed", http.StatusFound)
	}))
	defer api.Close()
	body, _, err := newTestClient(t, api.URL).JobLog(context.Background(), 5, "o", "r", 7)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	n, err := io.Copy(io.Discard, body)
	if err != nil || n != 9<<20 {
		t.Fatalf("read %d bytes, err=%v; want %d", n, err, 9<<20)
	}
	if apiRange != "" || blobRange != "" || blobAuth != "" {
		t.Fatalf("range api=%q blob=%q, blob auth=%q; want all empty", apiRange, blobRange, blobAuth)
	}
}

func TestJobLogMapsThrottlingAndOtherFailures(t *testing.T) {
	for _, tc := range []struct {
		status  int
		limited bool
	}{{http.StatusTooManyRequests, true}, {http.StatusNotFound, false}} {
		srv := httptest.NewServer(tokenOrElse(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte("ghs_secret"))
		}))
		_, _, err := newTestClient(t, srv.URL).JobLog(context.Background(), 5, "o", "r", 7)
		srv.Close()
		var rl *RateLimitError
		if err == nil || errors.As(err, &rl) != tc.limited || strings.Contains(err.Error(), "ghs_secret") {
			t.Fatalf("status %d: err=%v limited=%v", tc.status, err, tc.limited)
		}
	}
}

// The budget ledger learns from the rate headers of every call, so the
// client has to hand them back.
func TestListRunJobsReturnsTheRateLimitHeaders(t *testing.T) {
	srv := httptest.NewServer(tokenOrElse(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "1200")
		w.Header().Set("X-RateLimit-Reset", "1893456000")
		_, _ = w.Write([]byte(`{"jobs":[]}`))
	}))
	defer srv.Close()
	_, _, h, err := newTestClient(t, srv.URL).ListRunJobs(context.Background(), 5, "o", "r", 99, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if h.Get("X-RateLimit-Remaining") != "1200" || h.Get("X-RateLimit-Reset") != "1893456000" {
		t.Fatalf("headers = %v", h)
	}
}

// The signed blob host answers with its own headers. Only GitHub's API answer
// carries the installation's rate limit.
func TestJobLogReturnsTheAPIHeadersNotTheBlobHosts(t *testing.T) {
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "999999")
		_, _ = w.Write([]byte("log"))
	}))
	defer blob.Close()
	for name, redirect := range map[string]bool{"redirected": true, "direct": false} {
		api := httptest.NewServer(tokenOrElse(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "1200")
			w.Header().Set("X-RateLimit-Reset", "1893456000")
			if redirect {
				http.Redirect(w, r, blob.URL+"/signed", http.StatusFound)
				return
			}
			_, _ = w.Write([]byte("log"))
		}))
		body, h, err := newTestClient(t, api.URL).JobLog(context.Background(), 5, "o", "r", 7)
		api.Close()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_ = body.Close()
		if h.Get("X-RateLimit-Remaining") != "1200" || h.Get("X-RateLimit-Reset") != "1893456000" {
			t.Errorf("%s: headers = %v, want the API's 1200 / 1893456000", name, h)
		}
	}
}
