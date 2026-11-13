package ghsnapshot

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

const stackJSON = `{"id":1,"node_id":"PRS_x","url":"u","base":{"ref":"main"},"open":true,"created_at":"2026-11-05T11:28:32Z","pull_requests":[` +
	`{"number":3,"state":"closed","draft":false,"merged_at":"2026-11-05T11:29:24Z","head":{"ref":"a","sha":"s3"}},` +
	`{"number":6,"state":"open","draft":true,"merged_at":null,"head":{"ref":"b","sha":"s6"}}],"number":9}`

var wantStack = Stack{Number: 9, BaseRef: "main", Open: true, PullRequests: []StackPR{
	{Number: 3, State: "closed", MergedAt: "2026-11-05T11:29:24Z", HeadRef: "a", HeadSHA: "s3"},
	{Number: 6, State: "open", Draft: true, HeadRef: "b", HeadSHA: "s6"},
}}

type recorded struct{ method, path, query, body, auth string }

// stacksAPI answers the token mint and routes every other call to h, recording it.
func stacksAPI(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*Client, *recorded) {
	t.Helper()
	rec := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_secret","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		*rec = recorded{r.Method, r.URL.Path, r.URL.RawQuery, string(b), r.Header.Get("Authorization")}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return newTestClient(t, srv.URL), rec
}

func reply(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

type call struct {
	name string
	run  func(c *Client) error
}

// Every call goes through the same failure matrix.
func stackCalls() []call {
	ctx := context.Background()
	return []call{
		{"StacksForPR", func(c *Client) error { _, err := c.StacksForPR(ctx, 1, "o", "r", 6); return err }},
		{"GetStack", func(c *Client) error { _, err := c.GetStack(ctx, 1, "o", "r", 9); return err }},
		{"CreateStack", func(c *Client) error { _, err := c.CreateStack(ctx, 1, "o", "r", []int{3, 6}); return err }},
		{"AddToStack", func(c *Client) error { _, err := c.AddToStack(ctx, 1, "o", "r", 9, []int{7}); return err }},
	}
}

func TestStacksTypedErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		header map[string]string
		check  func(error) bool
	}{
		{"404", 404, `{"message":"Not Found"}`, nil, func(e error) bool { return errors.Is(e, ErrStacksUnsupported) }},
		{"403", 403, `{"message":"Resource not accessible by integration"}`, nil, func(e error) bool { return errors.Is(e, ErrStacksNoPermission) }},
		{"422", 422, `{"message":"Validation Failed"}`, nil, func(e error) bool { return errors.Is(e, ErrStacksConflict) }},
		{"unknown field", 200, `{"number":9,"base":{"ref":"main"},"open":true,"pull_requests":[],"surprise":1}`, nil, func(e error) bool { return errors.Is(e, ErrStacksSchema) }},
		{"missing pull_requests", 200, `{"number":9,"base":{"ref":"main"},"open":true}`, nil, func(e error) bool { return errors.Is(e, ErrStacksSchema) }},
		{"missing open", 200, `{"number":9,"base":{"ref":"main"},"pull_requests":[]}`, nil, func(e error) bool { return errors.Is(e, ErrStacksSchema) }},
		{"null open", 200, `{"number":9,"base":{"ref":"main"},"open":null,"pull_requests":[]}`, nil, func(e error) bool { return errors.Is(e, ErrStacksSchema) }},
		{"missing draft", 200, `{"number":9,"base":{"ref":"main"},"open":true,"pull_requests":[{"number":3,"state":"open","merged_at":null,"head":{"ref":"a","sha":"s3"}}]}`, nil, func(e error) bool { return errors.Is(e, ErrStacksSchema) }},
		{"null draft", 200, `{"number":9,"base":{"ref":"main"},"open":true,"pull_requests":[{"number":3,"state":"open","draft":null,"merged_at":null,"head":{"ref":"a","sha":"s3"}}]}`, nil, func(e error) bool { return errors.Is(e, ErrStacksSchema) }},
		{"missing merged_at", 200, `{"number":9,"base":{"ref":"main"},"open":true,"pull_requests":[{"number":3,"state":"open","draft":false,"head":{"ref":"a","sha":"s3"}}]}`, nil, func(e error) bool { return errors.Is(e, ErrStacksSchema) }},
		{"not json", 200, `<html>`, nil, func(e error) bool { return errors.Is(e, ErrStacksSchema) }},
		{"rate limit", 403, `{"message":"rate limit"}`, map[string]string{"X-RateLimit-Remaining": "0", "Retry-After": "7"}, func(e error) bool {
			var rl *RateLimitError
			return errors.As(e, &rl) && rl.RetryAfter == 7*time.Second
		}},
	}
	for _, call := range stackCalls() {
		for _, tc := range cases {
			t.Run(call.name+"/"+tc.name, func(t *testing.T) {
				status := tc.status
				if status == 200 && (call.name == "CreateStack" || call.name == "AddToStack") {
					status = 201
				}
				body := tc.body
				if call.name == "StacksForPR" && strings.HasPrefix(body, "{") && status == 200 {
					body = "[" + body + "]" // the list endpoint wraps stacks in an array
				}
				c, _ := stacksAPI(t, func(w http.ResponseWriter, _ *http.Request) {
					for k, v := range tc.header {
						w.Header().Set(k, v)
					}
					w.WriteHeader(status)
					_, _ = w.Write([]byte(body))
				})
				err := call.run(c)
				if err == nil || !tc.check(err) {
					t.Fatalf("err = %v", err)
				}
				if strings.Contains(err.Error(), "ghs_secret") || strings.Contains(err.Error(), tc.body) {
					t.Fatalf("error leaks token or body: %v", err)
				}
			})
		}
	}
}

func TestStacksForPR(t *testing.T) {
	c, rec := stacksAPI(t, reply(200, "["+stackJSON+"]"))
	got, err := c.StacksForPR(context.Background(), 1, "o", "r", 6)
	if err != nil || !reflect.DeepEqual(got, []Stack{wantStack}) {
		t.Fatalf("got %+v, err %v", got, err)
	}
	if rec.method != "GET" || rec.path != "/repos/o/r/stacks" || rec.query != "pull_request=6" || rec.auth != "Bearer ghs_secret" {
		t.Fatalf("request = %+v", rec)
	}
	c, _ = stacksAPI(t, reply(200, "[]"))
	if got, err := c.StacksForPR(context.Background(), 1, "o", "r", 6); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty list: got %+v, err %v", got, err)
	}
}

func TestGetStack(t *testing.T) {
	c, rec := stacksAPI(t, reply(200, stackJSON))
	got, err := c.GetStack(context.Background(), 1, "o", "r", 9)
	if err != nil || !reflect.DeepEqual(got, wantStack) {
		t.Fatalf("got %+v, err %v", got, err)
	}
	if rec.method != "GET" || rec.path != "/repos/o/r/stacks/9" {
		t.Fatalf("request = %+v", rec)
	}
}

func TestCreateStackPayload(t *testing.T) {
	c, rec := stacksAPI(t, reply(201, stackJSON))
	got, err := c.CreateStack(context.Background(), 1, "o", "r", []int{3, 6})
	if err != nil || !reflect.DeepEqual(got, wantStack) {
		t.Fatalf("got %+v, err %v", got, err)
	}
	if rec.method != "POST" || rec.path != "/repos/o/r/stacks" || rec.body != `{"pull_requests":[3,6]}` {
		t.Fatalf("request = %+v", rec)
	}
}

// Order is the contract: bottom first, exactly as given.
func TestAddToStackPayloadKeepsBottomFirstOrder(t *testing.T) {
	c, rec := stacksAPI(t, reply(200, stackJSON))
	if _, err := c.AddToStack(context.Background(), 1, "o", "r", 9, []int{12, 7, 30}); err != nil {
		t.Fatal(err)
	}
	if rec.method != "POST" || rec.path != "/repos/o/r/stacks/9/add" || rec.body != `{"pull_requests":[12,7,30]}` {
		t.Fatalf("request = %+v", rec)
	}
}

func TestStackWritesRejectBadPRList(t *testing.T) {
	c, rec := stacksAPI(t, reply(201, stackJSON))
	for _, prs := range [][]int{nil, {}, {0}, {5, 5}} {
		if _, err := c.CreateStack(context.Background(), 1, "o", "r", prs); err == nil {
			t.Fatalf("CreateStack(%v) succeeded", prs)
		}
		if _, err := c.AddToStack(context.Background(), 1, "o", "r", 9, prs); err == nil {
			t.Fatalf("AddToStack(%v) succeeded", prs)
		}
	}
	if rec.method != "" {
		t.Fatalf("invalid list reached the API: %+v", rec)
	}
}

func TestStacksNilClientDisabled(t *testing.T) {
	var c *Client
	for _, call := range stackCalls() {
		if err := call.run(c); err == nil {
			t.Fatalf("%s on nil client succeeded", call.name)
		}
	}
}
