package ghsnapshot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func variablesServer(t *testing.T, status int, pages ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"t","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
			return
		}
		if r.URL.Path != "/repos/o/r/actions/variables" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		var page int
		_, _ = fmt.Sscan(r.URL.Query().Get("page"), &page)
		_, _ = w.Write([]byte(pages[page-1]))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRepoVariableNamesListsEveryPage(t *testing.T) {
	var full []string
	for i := 0; i < snapshotPageSize; i++ {
		full = append(full, fmt.Sprintf(`{"name":"V%d"}`, i))
	}
	srv := variablesServer(t, http.StatusOK,
		`{"total_count":101,"variables":[`+strings.Join(full, ",")+`]}`,
		`{"total_count":101,"variables":[{"name":"AGENT_MERGE_PAUSED"}]}`)
	names, err := newTestClient(t, srv.URL).RepoVariableNames(context.Background(), 1, "o", "r")
	if err != nil || len(names) != snapshotPageSize+1 || names[len(names)-1] != "AGENT_MERGE_PAUSED" {
		t.Fatalf("names=%d err=%v", len(names), err)
	}
}

func TestRepoVariableNamesNamesTheMissingPermission(t *testing.T) {
	srv := variablesServer(t, http.StatusForbidden)
	_, err := newTestClient(t, srv.URL).RepoVariableNames(context.Background(), 1, "o", "r")
	if !errors.Is(err, ErrVariablesPermission) {
		t.Fatalf("err = %v, want ErrVariablesPermission", err)
	}
}

func TestRepoVariableNamesRejectsAMalformedPage(t *testing.T) {
	srv := variablesServer(t, http.StatusOK, `{"total_count":0}`)
	if _, err := newTestClient(t, srv.URL).RepoVariableNames(context.Background(), 1, "o", "r"); err == nil {
		t.Fatal("a body without variables was read as an empty list")
	}
}
