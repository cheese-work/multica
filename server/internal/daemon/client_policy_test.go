package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

// The broker's Policy reads the server record live on every push, so an admin
// edit between two pushes applies to the second one, and a failed read is an
// error (the broker refuses on it), never a cached or empty answer.
func TestBrokerPolicyReadsLive(t *testing.T) {
	var edit atomic.Value
	edit.Store(`{"name":"default","patterns":[".github/**"]}`)
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/workspaces/ws-1/protected-path-policies/default" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if s := int(status.Load()); s != http.StatusOK {
			http.Error(w, "boom", s)
			return
		}
		_, _ = w.Write([]byte(edit.Load().(string)))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	c.SetToken("tok")
	policy := c.BrokerPolicy("ws-1")

	got, err := policy(context.Background())
	if err != nil || !reflect.DeepEqual(got, []string{".github/**"}) {
		t.Fatalf("first read = %v, %v", got, err)
	}
	edit.Store(`{"name":"default","patterns":["secret/**","docs/**"]}`)
	got, err = policy(context.Background())
	if err != nil || !reflect.DeepEqual(got, []string{"secret/**", "docs/**"}) {
		t.Fatalf("read after admin edit = %v, %v; want the edit", got, err)
	}
	status.Store(http.StatusInternalServerError)
	if got, err = policy(context.Background()); err == nil {
		t.Fatalf("failed lookup returned %v, want an error", got)
	}
}
