package githubrepo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeGitHub struct {
	calls  []string
	bodies []map[string]any
	auth   []string
	exists bool
}

func (fake *fakeGitHub) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	fake.calls = append(fake.calls, request.Method+" "+request.URL.Path)
	fake.auth = append(fake.auth, request.Header.Get("Authorization"))
	var body map[string]any
	raw, _ := io.ReadAll(request.Body)
	_ = json.Unmarshal(raw, &body)
	fake.bodies = append(fake.bodies, body)
	switch {
	case request.Method == http.MethodPost && fake.exists:
		writer.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = writer.Write([]byte(`{"message":"Repository creation failed.","errors":[{"message":"name already exists on this account"}]}`))
	case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/generate") && strings.Contains(request.URL.Path, "missing-template"):
		writer.WriteHeader(http.StatusNotFound)
	case request.Method == http.MethodPost:
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{}`))
	default:
		_, _ = writer.Write([]byte(`{"full_name":"tevo-studio/we006","html_url":"https://github.com/tevo-studio/we006","private":true,"visibility":"private"}`))
	}
}

func newFake(test *testing.T) (*Client, *fakeGitHub) {
	test.Helper()
	fake := &fakeGitHub{}
	server := httptest.NewServer(fake)
	test.Cleanup(server.Close)
	return NewClient(server.URL, map[string]string{"Tevo-Studio": "secret-token"}), fake
}

func TestCreateRepoFromTemplateUsesTheOrgTokenAndReadsBack(test *testing.T) {
	client, fake := newFake(test)
	created, err := client.CreateRepo(context.Background(), Request{Org: "tevo-studio", Name: "we006", Visibility: "private", Template: "tevo-studio/code-android-template", Description: "d"})
	if err != nil {
		test.Fatal(err)
	}
	if created.FullName != "tevo-studio/we006" || created.Visibility != "private" {
		test.Fatalf("created = %+v", created)
	}
	if got := fake.calls[0]; got != "POST /repos/tevo-studio/code-android-template/generate" {
		test.Fatalf("first call = %s", got)
	}
	if fake.bodies[0]["private"] != true || fake.bodies[0]["owner"] != "tevo-studio" {
		test.Fatalf("body = %v", fake.bodies[0])
	}
	if fake.auth[0] != "Bearer secret-token" || fake.calls[1] != "GET /repos/tevo-studio/we006" {
		test.Fatalf("auth %q calls %v", fake.auth[0], fake.calls)
	}
}

func TestCreateRepoWithoutTemplateUsesTheOrgEndpoint(test *testing.T) {
	client, fake := newFake(test)
	if _, err := client.CreateRepo(context.Background(), Request{Org: "tevo-studio", Name: "we006", Visibility: "private"}); err != nil {
		test.Fatal(err)
	}
	if fake.calls[0] != "POST /orgs/tevo-studio/repos" || fake.bodies[0]["visibility"] != "private" {
		test.Fatalf("calls %v body %v", fake.calls, fake.bodies[0])
	}
}

func TestCreateRepoRefusals(test *testing.T) {
	client, fake := newFake(test)
	if _, err := client.CreateRepo(context.Background(), Request{Org: "cheese-work", Name: "x", Visibility: "private"}); !errors.Is(err, ErrNotConfigured) {
		test.Fatalf("no token: %v", err)
	}
	if len(fake.calls) != 0 {
		test.Fatalf("an org without a token reached GitHub: %v", fake.calls)
	}
	if _, err := client.CreateRepo(context.Background(), Request{Org: "tevo-studio", Name: "x", Visibility: "private", Template: "tevo-studio/missing-template"}); !errors.Is(err, ErrTemplate) {
		test.Fatalf("template: %v", err)
	}
	fake.exists = true
	if _, err := client.CreateRepo(context.Background(), Request{Org: "tevo-studio", Name: "x", Visibility: "private"}); !errors.Is(err, ErrExists) {
		test.Fatalf("exists: %v", err)
	}
}

func TestErrorsNeverCarryTheToken(test *testing.T) {
	client := NewClient("http://127.0.0.1:1", map[string]string{"tevo-studio": "secret-token"})
	_, err := client.CreateRepo(context.Background(), Request{Org: "tevo-studio", Name: "x", Visibility: "private"})
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		test.Fatalf("err = %v", err)
	}
}

func TestNewClientFromEnv(test *testing.T) {
	client, err := NewClientFromEnv(func(string) string { return `{"tevo-studio":"t"}` })
	if err != nil || client.tokens["tevo-studio"] != "t" {
		test.Fatalf("client %+v err %v", client, err)
	}
	bad, err := NewClientFromEnv(func(string) string { return "not-json-token-value" })
	if err == nil || strings.Contains(err.Error(), "not-json-token-value") || len(bad.tokens) != 0 {
		test.Fatalf("malformed value: err %v tokens %v", err, bad.tokens)
	}
}

func TestApprovedOrgs(test *testing.T) {
	if !IsApprovedOrg("Tevo-Studio") || !IsApprovedOrg("cheese-work") || IsApprovedOrg("evil-org") {
		test.Fatal("approved org table is wrong")
	}
}
