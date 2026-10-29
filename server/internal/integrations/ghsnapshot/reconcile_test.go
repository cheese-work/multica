package ghsnapshot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestListHookDeliveriesUsesAppJWTAndOnlyTheCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/app/hook/deliveries" || !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ey") || request.URL.Query().Get("cursor") != "abc-1" {
			t.Errorf("unexpected request %s auth=%q", request.URL.String(), request.Header.Get("Authorization"))
		}
		writer.Header().Set("Link", `<https://untrusted.invalid/app/hook/deliveries?per_page=100&cursor=next-2>; rel="next"`)
		fmt.Fprint(writer, `[{"id":7,"guid":"g-1","delivered_at":"2026-10-28T01:00:00Z","event":"issue_comment","status_code":502}]`)
	}))
	defer server.Close()
	attempts, next, err := newTestClient(t, server.URL).ListHookDeliveries(context.Background(), "abc-1")
	if err != nil || next != "next-2" || len(attempts) != 1 || attempts[0].ID != 7 || attempts[0].Succeeded() {
		t.Fatalf("attempts=%+v next=%q err=%v", attempts, next, err)
	}
}

func TestRedeliverHookRequiresAccepted(t *testing.T) {
	for status, wantErr := range map[int]bool{http.StatusAccepted: false, http.StatusOK: true, http.StatusNotFound: true} {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method != http.MethodPost || request.URL.Path != "/app/hook/deliveries/7/attempts" {
				t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
			}
			writer.WriteHeader(status)
		}))
		err := newTestClient(t, server.URL).RedeliverHook(context.Background(), 7)
		server.Close()
		if (err != nil) != wantErr {
			t.Errorf("status %d: err=%v", status, err)
		}
	}
	var client *Client
	if client.RedeliverHook(context.Background(), 7) == nil {
		t.Fatal("disabled client accepted")
	}
}

func TestListPullObjectsFiltersAndGuardsForks(t *testing.T) {
	fork := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/app/installations/42/access_tokens":
			writer.WriteHeader(http.StatusCreated)
			fmt.Fprintf(writer, `{"token":"t","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case "/repos/acme/widget/pulls/5":
			head := 9001
			if fork {
				head = 1
			}
			fmt.Fprintf(writer, `{"state":"open","head":{"sha":"h","repo":{"id":%d}},"base":{"repo":{"id":9001}}}`, head)
		case "/repos/acme/widget/pulls/5/reviews":
			fmt.Fprint(writer, `[{"id":1,"state":"APPROVED","body":null,"user":{"id":42,"login":"r","type":"User"}},{"id":2,"state":"PENDING","body":"x","user":{"id":42,"login":"r","type":"User"}},{"id":3,"state":"DISMISSED","body":"x","user":{"id":42,"login":"r","type":"User"}}]`)
		case "/repos/acme/widget/pulls/5/comments":
			fmt.Fprint(writer, `[{"id":4,"body":"inline","created_at":"2026-10-28T01:00:00Z","user":{"id":42,"login":"r","type":"User"}}]`)
		case "/repos/acme/widget/issues/5/comments":
			fmt.Fprint(writer, `[{"id":5,"body":"talk","user":null}]`)
		default:
			t.Errorf("unexpected path %s", request.URL.Path)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	pull, err := client.ListPullObjects(context.Background(), 42, "acme", "widget", 5)
	if err != nil || pull.RepositoryID != 9001 || !pull.Open || !pull.SameRepo || len(pull.Objects) != 2 ||
		pull.Objects[0].Kind != "review" || pull.Objects[0].ReviewState != "approved" || pull.Objects[1].ID != 4 {
		t.Fatalf("pull=%+v err=%v", pull, err)
	}
	fork = true
	pull, err = client.ListPullObjects(context.Background(), 42, "acme", "widget", 5)
	if err != nil || pull.SameRepo || len(pull.Objects) != 0 {
		t.Fatalf("fork PR objects were read: %+v err=%v", pull, err)
	}
}
