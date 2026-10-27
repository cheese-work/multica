package ghsnapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetchReviewCommentsPaginatesOnFixedAuthenticatedPath(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/access_tokens") {
			writer.WriteHeader(http.StatusCreated)
			fmt.Fprintf(writer, `{"token":"test-review-token","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			return
		}
		if request.URL.Path != "/repos/acme/widget/pulls/2019/reviews/77/comments" || request.Header.Get("Authorization") != "Bearer test-review-token" || request.URL.Query().Get("per_page") != "100" {
			t.Errorf("unexpected review request: %s", request.URL.Path)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		pages++
		entries := make([]ReviewComment, 100)
		if request.URL.Query().Get("page") == "2" {
			entries = []ReviewComment{{ID: 101, Body: "last inline comment"}}
		} else if request.URL.Query().Get("page") != "1" {
			t.Errorf("unexpected review page: %s", request.URL.RawQuery)
		}
		writer.Header().Set("Link", `<https://untrusted.invalid/steal>; rel="next"`)
		json.NewEncoder(writer).Encode(entries)
	}))
	defer server.Close()
	comments, err := newTestClient(t, server.URL).FetchReviewComments(context.Background(), 42, "acme", "widget", 2019, 77)
	if err != nil {
		t.Fatal(err)
	}
	if pages != 2 || len(comments) != 101 || comments[100].Body != "last inline comment" {
		t.Fatalf("review pagination: pages=%d, comments=%d", pages, len(comments))
	}
}

func TestFetchReviewCommentsFailsClosed(t *testing.T) {
	for _, body := range []string{"null", "{}", "invalid", `[{"body":"` + strings.Repeat("x", 1<<20+1) + `"}]`} {
		t.Run(fmt.Sprint(len(body), body[:min(len(body), 12)]), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if strings.HasSuffix(request.URL.Path, "/access_tokens") {
					writer.WriteHeader(http.StatusCreated)
					fmt.Fprintf(writer, `{"token":"test-review-token","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
					return
				}
				fmt.Fprint(writer, body)
			}))
			defer server.Close()
			if _, err := newTestClient(t, server.URL).FetchReviewComments(context.Background(), 42, "acme", "widget", 2019, 77); err == nil {
				t.Fatal("malformed or oversized review comments accepted")
			}
		})
	}
	var client *Client
	if _, err := client.FetchReviewComments(context.Background(), 42, "acme", "widget", 2019, 77); err == nil {
		t.Fatal("disabled review API accepted")
	}
}
