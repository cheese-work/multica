package jev

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeBaseURLAndAllowlist(t *testing.T) {
	baseURL, authority, err := normalizeBaseURL("HTTPS://API.Typesafe.AI.:0443/v1/")
	if err != nil {
		t.Fatalf("normalizeBaseURL: %v", err)
	}
	if authority != "api.typesafe.ai:443" {
		t.Errorf("authority = %q, want normalized host:port", authority)
	}
	if baseURL != "https://api.typesafe.ai:443/v1" {
		t.Errorf("baseURL = %q", baseURL)
	}

	allowed, err := loadEgressAllowlist(" API.Typesafe.AI.:0443 ", nil)
	if err != nil {
		t.Fatalf("loadEgressAllowlist: %v", err)
	}
	if _, ok := allowed[authority]; !ok {
		t.Errorf("normalized authority %q is not allowlisted", authority)
	}
}

func TestNormalizeBaseURLRejectsUnsafeForms(t *testing.T) {
	for _, raw := range []string{
		"http://api.typesafe.ai/v1",
		"https://user:secret@api.typesafe.ai/v1",
		"https://api.typesafe.ai/v1?key=secret",
		"https://api.typesafe.ai/v1#fragment",
		"https://api.typesafe.ai:0/v1",
		"https://api.typesafe.ai:70000/v1",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, _, err := normalizeBaseURL(raw); err == nil {
				t.Fatalf("normalizeBaseURL(%q) succeeded", raw)
			} else if strings.Contains(err.Error(), "secret") {
				t.Fatalf("validation error disclosed input: %v", err)
			}
		})
	}
}

func TestNewClientRejectsUnauthorizedAuthorityBeforeDNS(t *testing.T) {
	var lookups atomic.Int32
	var dials atomic.Int32
	t.Setenv(EgressAllowlistEnv, "provider.example:443")
	_, err := NewClient(Options{
		APIKey:  "test-key",
		BaseURL: "https://provider.example:8443",
		lookupIP: func(context.Context, string) ([]net.IP, error) {
			lookups.Add(1)
			return []net.IP{net.ParseIP("8.8.8.8")}, nil
		},
		dialContext: func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("unexpected dial")
		},
	})
	if !errors.Is(err, ErrEgressDenied) {
		t.Fatalf("NewClient error = %v, want ErrEgressDenied", err)
	}
	if lookups.Load() != 0 || dials.Load() != 0 {
		t.Fatalf("unauthorized authority triggered DNS/dial: %d/%d", lookups.Load(), dials.Load())
	}
}

func TestMetadataHostnameIsDeniedBeforeDNS(t *testing.T) {
	var lookups atomic.Int32
	var dials atomic.Int32
	t.Setenv(EgressAllowlistEnv, "metadata.google.internal:443")
	client, err := NewClient(Options{
		APIKey:  "test-key",
		BaseURL: "https://metadata.google.internal",
		lookupIP: func(context.Context, string) ([]net.IP, error) {
			lookups.Add(1)
			return []net.IP{net.ParseIP("8.8.8.8")}, nil
		},
		dialContext: func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("unexpected dial")
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.Evaluate(context.Background(), Request{State: "s", Questions: map[string]Question{"q": NewNoul("q?", nil)}})
	if !errors.Is(err, ErrEgressDenied) {
		t.Fatalf("Evaluate error = %v, want ErrEgressDenied", err)
	}
	if lookups.Load() != 0 || dials.Load() != 0 {
		t.Fatalf("metadata destination triggered DNS/dial: %d/%d", lookups.Load(), dials.Load())
	}
}

func TestPinnedDialContextRevalidatesAndPinsAddresses(t *testing.T) {
	allowed, err := loadEgressAllowlist("provider.example:443", nil)
	if err != nil {
		t.Fatal(err)
	}
	lookupCount := 0
	var dialed string
	dial := newPinnedDialContext(allowed, func(context.Context, string) ([]net.IP, error) {
		lookupCount++
		if lookupCount == 1 {
			return []net.IP{net.ParseIP("8.8.8.8")}, nil
		}
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}, func(_ context.Context, network, address string) (net.Conn, error) {
		dialed = network + ":" + address
		return nil, errors.New("fake socket refusal")
	})

	_, firstErr := dial(context.Background(), "tcp", "provider.example:443")
	if !errors.Is(firstErr, ErrUpstreamRequest) {
		t.Fatalf("first dial error = %v, want sanitized connection failure", firstErr)
	}
	if dialed != "tcp:8.8.8.8:443" {
		t.Fatalf("dial target = %q, want pinned resolved IP", dialed)
	}
	_, secondErr := dial(context.Background(), "tcp", "provider.example:443")
	if !errors.Is(secondErr, ErrEgressDenied) {
		t.Fatalf("rebound dial error = %v, want ErrEgressDenied", secondErr)
	}
	if lookupCount != 2 || dialed != "tcp:8.8.8.8:443" {
		t.Fatalf("revalidation lookup/dial = %d/%q", lookupCount, dialed)
	}
}

func TestPinnedDialContextRejectsMixedDNSAnswersBeforeDial(t *testing.T) {
	allowed, _ := loadEgressAllowlist("provider.example:443", nil)
	dialCount := 0
	dial := newPinnedDialContext(allowed, func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("169.254.169.254")}, nil
	}, func(context.Context, string, string) (net.Conn, error) {
		dialCount++
		return nil, errors.New("unexpected dial")
	})
	_, err := dial(context.Background(), "tcp", "provider.example:443")
	if !errors.Is(err, ErrEgressDenied) {
		t.Fatalf("dial error = %v, want ErrEgressDenied", err)
	}
	if dialCount != 0 {
		t.Fatalf("mixed public/private DNS answers caused %d dial(s)", dialCount)
	}
}

func TestPrivateAndMetadataAddressPolicy(t *testing.T) {
	for _, test := range []struct {
		name    string
		address string
		literal bool
		want    bool
	}{
		{name: "explicit loopback", address: "127.0.0.1", literal: true, want: false},
		{name: "DNS to loopback", address: "127.0.0.1", want: true},
		{name: "private DNS answer", address: "10.1.2.3", want: true},
		{name: "metadata IPv4", address: "169.254.169.254", literal: true, want: true},
		{name: "metadata IPv6", address: "fd00:ec2::254", literal: true, want: true},
		{name: "documentation range", address: "203.0.113.8", literal: true, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := forbiddenAddress(net.ParseIP(test.address), test.literal); got != test.want {
				t.Errorf("forbiddenAddress(%s, %t) = %t, want %t", test.address, test.literal, got, test.want)
			}
		})
	}
}

func TestClientVerifiesTLSHostname(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeJSON(w, http.StatusOK, `{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.5}},"usage":{}}`)
	}))
	t.Cleanup(srv.Close)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	portText := strconv.Itoa(port)
	t.Setenv(EgressAllowlistEnv, "provider.example:"+portText)
	client, err := NewClient(Options{
		APIKey:     "test-key",
		BaseURL:    "https://provider.example:" + portText,
		HTTPClient: srv.Client(),
		RetryCount: -1,
		lookupIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("8.8.8.8")}, nil
		},
		dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.Evaluate(context.Background(), Request{State: "s", Questions: map[string]Question{"q": NewNoul("q?", nil)}})
	if !errors.Is(err, ErrUpstreamRequest) {
		t.Fatalf("Evaluate error = %v, want sanitized TLS hostname failure", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("hostname mismatch reached server %d time(s)", hits.Load())
	}
	if strings.Contains(err.Error(), "provider.example") {
		t.Fatalf("TLS error disclosed upstream details: %v", err)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	var sourceHits atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		sourceHits.Add(1)
		http.Redirect(w, r, target.URL, http.StatusFound)
	}, nil)
	_, err := client.Evaluate(context.Background(), Request{State: "s", Questions: map[string]Question{"q": NewNoul("q?", nil)}})
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.HTTPStatus != http.StatusFound {
		t.Fatalf("Evaluate error = %v, want non-followed 302 APIError", err)
	}
	if sourceHits.Load() != 1 || targetHits.Load() != 0 {
		t.Fatalf("source/redirect target hits = %d/%d, want 1/0", sourceHits.Load(), targetHits.Load())
	}
}

func TestResponseSizeAndTimeoutAreBounded(t *testing.T) {
	t.Run("response body", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", maxResponseBytes+1)))
		}, nil)
		_, err := client.Evaluate(context.Background(), Request{State: "s", Questions: map[string]Question{"q": NewNoul("q?", nil)}})
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("Evaluate error = %v, want ErrResponseTooLarge", err)
		}
	})

	t.Run("request timeout", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(100 * time.Millisecond):
			}
			w.WriteHeader(http.StatusOK)
		}, func(opts *Options) { opts.Timeout = 20 * time.Millisecond })
		started := time.Now()
		_, err := client.Evaluate(context.Background(), Request{State: "s", Questions: map[string]Question{"q": NewNoul("q?", nil)}})
		if !errors.Is(err, ErrUpstreamRequest) {
			t.Fatalf("Evaluate error = %v, want sanitized timeout", err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("bounded request took %s", elapsed)
		}
	})
}

func TestConfiguredTimeoutCannotDisableOrExceedBound(t *testing.T) {
	t.Setenv(EgressAllowlistEnv, "api.typesafe.ai:443")
	client, err := NewClient(Options{APIKey: "test-key", Timeout: time.Hour})
	if err != nil {
		t.Fatalf("NewClient with long timeout: %v", err)
	}
	if client.timeout != maxRequestTimeout {
		t.Errorf("timeout = %s, want max %s", client.timeout, maxRequestTimeout)
	}
	if _, err := NewClient(Options{APIKey: "test-key", Timeout: -time.Second}); err == nil {
		t.Fatal("NewClient accepted a timeout that disables the bound")
	}
}

func TestTransportErrorsAreScrubbed(t *testing.T) {
	t.Setenv(EgressAllowlistEnv, "provider.example:443")
	client, err := NewClient(Options{
		APIKey:     "test-key",
		BaseURL:    "https://provider.example",
		RetryCount: -1,
		lookupIP: func(context.Context, string) ([]net.IP, error) {
			return nil, errors.New("upstream leaked Bearer test-key")
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.Evaluate(context.Background(), Request{State: "s", Questions: map[string]Question{"q": NewNoul("q?", nil)}})
	if !errors.Is(err, ErrUpstreamRequest) {
		t.Fatalf("Evaluate error = %v, want ErrUpstreamRequest", err)
	}
	if strings.Contains(err.Error(), "test-key") || strings.Contains(err.Error(), "provider.example") {
		t.Fatalf("transport error disclosed details: %v", err)
	}
}

func TestRetryLoggingDoesNotExposeUpstreamDecodeError(t *testing.T) {
	const fakeSecret = "987654321987654321987654321"
	logReader, logWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	originalStderr := os.Stderr
	os.Stderr = logWriter
	t.Cleanup(func() {
		os.Stderr = originalStderr
		_ = logWriter.Close()
		_ = logReader.Close()
	})

	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"input_tokens":`+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")+`}}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(EgressAllowlistEnv, srv.Listener.Addr().String())
	client, err := NewClient(Options{
		APIKey:           fakeSecret,
		BaseURL:          srv.URL,
		HTTPClient:       srv.Client(),
		RetryCount:       1,
		RetryWaitTime:    time.Millisecond,
		RetryMaxWaitTime: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, evaluateErr := client.Evaluate(context.Background(), Request{
		State:     "fake",
		Questions: map[string]Question{"q": NewNoul("fake?", nil)},
	})
	os.Stderr = originalStderr
	if err := logWriter.Close(); err != nil {
		t.Fatalf("close captured log writer: %v", err)
	}
	logs, err := io.ReadAll(logReader)
	if err != nil {
		t.Fatalf("read captured logs: %v", err)
	}
	if err := logReader.Close(); err != nil {
		t.Fatalf("close captured log reader: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("fake upstream hits = %d, want 2 attempts", hits.Load())
	}
	if !errors.Is(evaluateErr, ErrUpstreamRequest) {
		t.Fatalf("Evaluate error = %v, want ErrUpstreamRequest", evaluateErr)
	}
	if strings.Contains(evaluateErr.Error(), fakeSecret) || bytes.Contains(logs, []byte(fakeSecret)) {
		t.Fatalf("fake credential leaked in error or logs: error=%q logs=%q", evaluateErr, logs)
	}
}

func TestResponseLimitAppliesAfterGzipDecompression(t *testing.T) {
	payload := []byte(`{"model":"` + strings.Repeat("x", 2<<20) + `","answers":{"q":{"type":"noul","noul":0.5}},"usage":{}}`)
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(payload); err != nil {
		t.Fatalf("gzip payload: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	if compressed.Len() >= maxResponseBytes || len(payload) <= maxResponseBytes {
		t.Fatalf("test payload sizes: compressed=%d inflated=%d", compressed.Len(), len(payload))
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(compressed.Bytes())
	}))
	t.Cleanup(srv.Close)
	t.Setenv(EgressAllowlistEnv, srv.Listener.Addr().String())

	for _, test := range []struct {
		name               string
		disableCompression bool
	}{
		{name: "ordinary transport"},
		{name: "compression disabled on supplied transport", disableCompression: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			httpClient := srv.Client()
			if test.disableCompression {
				transport := httpClient.Transport.(*http.Transport).Clone()
				transport.DisableCompression = true
				httpClient.Transport = transport
			}
			client, err := NewClient(Options{
				APIKey:     "fake-key",
				BaseURL:    srv.URL,
				HTTPClient: httpClient,
				RetryCount: -1,
			})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			response, err := client.Evaluate(context.Background(), Request{
				State:     "fake",
				Questions: map[string]Question{"q": NewNoul("fake?", nil)},
			})
			if !errors.Is(err, ErrResponseTooLarge) {
				t.Fatalf("Evaluate error = %v, want ErrResponseTooLarge", err)
			}
			if response != nil {
				t.Fatalf("Evaluate returned oversized response with model length %d", len(response.Model))
			}
		})
	}
}
