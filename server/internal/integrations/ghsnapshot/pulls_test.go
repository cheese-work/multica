package ghsnapshot

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"reflect"
	"testing"
)

func TestNewClientWithAPIBaseIsEnabled(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	c := NewClientWithAPIBase("1", key, "http://x")
	if !c.Enabled() {
		t.Fatal("client built with a key must be enabled")
	}
}

func TestDefaultBranch(t *testing.T) {
	c, rec := stacksAPI(t, reply(http.StatusOK, `{"default_branch":"trunk","name":"r"}`))
	got, err := c.DefaultBranch(context.Background(), 1, "o", "r")
	if err != nil || got != "trunk" {
		t.Fatalf("got %q, %v", got, err)
	}
	if rec.method != "GET" || rec.path != "/repos/o/r" {
		t.Fatalf("request %+v", rec)
	}
	empty, _ := stacksAPI(t, reply(http.StatusOK, `{"name":"r"}`))
	if _, err := empty.DefaultBranch(context.Background(), 1, "o", "r"); err == nil {
		t.Fatal("missing default_branch must be an error")
	}
}

const prJSON = `{"number":7,"state":"open","merged":false,"head":{"ref":"feat","repo":{"full_name":"o/r"}},"base":{"ref":"main"}}`

func TestPullByNumber(t *testing.T) {
	c, rec := stacksAPI(t, reply(http.StatusOK, prJSON))
	got, err := c.PullByNumber(context.Background(), 1, "o", "r", 7)
	want := PullRef{Number: 7, Head: "feat", Base: "main", State: "open"}
	if err != nil || got != want {
		t.Fatalf("got %+v, %v", got, err)
	}
	if rec.path != "/repos/o/r/pulls/7" {
		t.Fatalf("path %q", rec.path)
	}
	fork, _ := stacksAPI(t, reply(http.StatusOK, `{"number":7,"state":"closed","merged":true,"head":{"ref":"f","repo":{"full_name":"x/r"}},"base":{"ref":"main"}}`))
	got, err = fork.PullByNumber(context.Background(), 1, "o", "r", 7)
	if err != nil || !got.Fork || !got.Merged || got.State != "closed" {
		t.Fatalf("fork/merged flags wrong: %+v, %v", got, err)
	}
	// A deleted fork has a null head repo: still a fork.
	gone, _ := stacksAPI(t, reply(http.StatusOK, `{"number":7,"state":"open","head":{"ref":"f","repo":null},"base":{"ref":"main"}}`))
	got, err = gone.PullByNumber(context.Background(), 1, "o", "r", 7)
	if err != nil || !got.Fork {
		t.Fatalf("null head repo must count as fork: %+v, %v", got, err)
	}
}

func TestPullsByHead(t *testing.T) {
	c, rec := stacksAPI(t, reply(http.StatusOK, `[`+prJSON+`]`))
	got, err := c.PullsByHead(context.Background(), 1, "o", "r", "feat")
	if err != nil || !reflect.DeepEqual(got, []PullRef{{Number: 7, Head: "feat", Base: "main", State: "open"}}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	if rec.path != "/repos/o/r/pulls" || rec.query != "head=o%3Afeat&per_page=100&state=all" {
		t.Fatalf("request %q ? %q", rec.path, rec.query)
	}
}

func TestPullsErrorsAreTyped(t *testing.T) {
	c, _ := stacksAPI(t, reply(http.StatusOK, `{"not":"a list"}`))
	if _, err := c.PullsByHead(context.Background(), 1, "o", "r", "feat"); err == nil {
		t.Fatal("non-list body must be an error")
	}
	nf, _ := stacksAPI(t, reply(http.StatusNotFound, `{}`))
	if _, err := nf.PullByNumber(context.Background(), 1, "o", "r", 7); err == nil {
		t.Fatal("404 must be an error")
	}
}
