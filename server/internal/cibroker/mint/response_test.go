package mint

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWriteTokenIsNoStore(t *testing.T) {
	w := httptest.NewRecorder()
	WriteToken(w, Token{Value: "ghs_abc", ExpiresAt: t0.Add(time.Hour), Permissions: Permissions{"contents": "read"}})
	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := res.Header.Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma = %q", got)
	}
	var body struct {
		Token       string            `json:"token"`
		ExpiresAt   time.Time         `json:"expires_at"`
		Permissions map[string]string `json:"permissions"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Token != "ghs_abc" || body.Permissions["contents"] != "read" {
		t.Fatalf("body = %+v", body)
	}
}

func TestWriteRefusalIsNoStoreWithRetryAfter(t *testing.T) {
	w := httptest.NewRecorder()
	WriteRefusal(w, &Refusal{Status: http.StatusTooManyRequests, Reason: "quota_core", RetryAfter: 90 * time.Second})
	res := w.Result()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", res.Header.Get("Cache-Control"))
	}
	if res.Header.Get("Retry-After") != "90" {
		t.Fatalf("Retry-After = %q", res.Header.Get("Retry-After"))
	}
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(res.Body).Decode(&body)
	if body.Error != "quota_core" {
		t.Fatalf("error = %q", body.Error)
	}
}
