package ghsnapshot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRateLimitReadsCoreBucket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_secret","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
		case r.URL.Path == "/rate_limit":
			_, _ = w.Write([]byte(`{"resources":{"core":{"limit":5000,"remaining":4321,"reset":1788700000},"search":{"limit":30,"remaining":1,"reset":1}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	remaining, reset, err := newTestClient(t, srv.URL).RateLimit(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 4321 || !reset.Equal(time.Unix(1788700000, 0)) {
		t.Fatalf("remaining=%d reset=%v", remaining, reset)
	}
}

func TestRateLimitRejectsBodyWithoutCoreBucket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"t","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{"resources":{}}`))
	}))
	defer srv.Close()
	if _, _, err := newTestClient(t, srv.URL).RateLimit(context.Background(), 42); err == nil {
		t.Fatal("want error for a body with no core bucket")
	}
}
