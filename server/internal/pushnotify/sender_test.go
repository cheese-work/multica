package pushnotify_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/pushnotify"
)

// Retrospective (written after the implementation): the real sender against a
// stand-in push service.

func testKeys(t *testing.T) (p256dh, auth string) {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(secret)
}

func TestWebPushSenderEncryptsAndSignsTheRequest(t *testing.T) {
	var got *http.Request
	var body int
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		buf := make([]byte, 8192)
		body, _ = r.Body.Read(buf)
		w.WriteHeader(http.StatusCreated)
	}))
	defer service.Close()
	p256dh, auth := testKeys(t)
	cfg := pushnotify.Config{
		PublicKey:  "BNcRdreALRFXTkOOUHK1EtK2wtaz5Ry4YfYCA_0QTpQtUbVlUls0VJXg7A8u-Ts1XbjhazAkj7I99e8QcYP7DkM",
		PrivateKey: "UUxI4O8-FbRouAevSmBQ6o18hgE4nSG3qwvJTfKc-ls",
		Subject:    "mailto:ops@example.com",
	}

	status, err := pushnotify.NewWebPushSender(cfg, http.DefaultClient).Send(context.Background(),
		pushnotify.Subscription{Endpoint: service.URL + "/send", P256dh: p256dh, Auth: auth}, []byte(`{"label":"New reply"}`))

	if err != nil || status != http.StatusCreated {
		t.Fatalf("status = %d, err = %v, want 201", status, err)
	}
	if got.Header.Get("Content-Encoding") != "aes128gcm" || got.Header.Get("TTL") == "" || !strings.HasPrefix(got.Header.Get("Authorization"), "vapid ") {
		t.Fatalf("headers = %v, want an RFC 8291 body signed with VAPID", got.Header)
	}
	if body == 0 {
		t.Fatal("empty body")
	}
}

func TestProductionClientRefusesNonPublicAddresses(t *testing.T) {
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the push client reached a loopback service")
	}))
	defer service.Close()
	p256dh, auth := testKeys(t)

	_, err := pushnotify.NewWebPushSender(pushnotify.Config{PublicKey: "BNcRdreALRFXTkOOUHK1EtK2wtaz5Ry4YfYCA_0QTpQtUbVlUls0VJXg7A8u-Ts1XbjhazAkj7I99e8QcYP7DkM", PrivateKey: "UUxI4O8-FbRouAevSmBQ6o18hgE4nSG3qwvJTfKc-ls", Subject: "mailto:ops@example.com"}, pushnotify.NewHTTPClient()).
		Send(context.Background(), pushnotify.Subscription{Endpoint: service.URL, P256dh: p256dh, Auth: auth}, []byte("{}"))

	if err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("err = %v, want the dial to be refused", err)
	}
}
