package credentialexec

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCredentialGatewayStreamLifecycle(test *testing.T) {
	for _, outcome := range []string{"complete", "empty", "truncated", "cancelled", "early-close", "upgrade"} {
		test.Run(outcome, func(test *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				if outcome == "empty" {
					writer.WriteHeader(http.StatusNoContent)
					return
				}
				if outcome == "upgrade" {
					writer.Header().Set("Connection", "Upgrade")
					writer.Header().Set("Upgrade", "owned-unsupported")
					writer.WriteHeader(http.StatusSwitchingProtocols)
					return
				}
				writer.Header().Set("Content-Type", "text/event-stream")
				if outcome == "truncated" {
					writer.Header().Set("Content-Length", "128")
				}
				_, _ = io.WriteString(writer, "data: owned partial output\n\n")
				if outcome == "cancelled" || outcome == "early-close" {
					writer.(http.Flusher).Flush()
					<-request.Context().Done()
				}
			}))
			defer upstream.Close()
			boundary := &Boundary{state: test.TempDir(), stopped: make(chan struct{})}
			marker := filepath.Join(boundary.state, "native-session")
			if err := os.WriteFile(marker, []byte("retained"), 0600); err != nil {
				test.Fatal(err)
			}
			transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
			defer transport.CloseIdleConnections()
			broker := &gatewayTransport{boundary: boundary, transport: transport}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+"/v1/responses", nil)
			if err != nil {
				test.Fatal(err)
			}
			response, err := broker.RoundTrip(request)
			if err != nil {
				test.Fatal(err)
			}
			defer response.Body.Close()
			if outcome != "upgrade" && boundary.requestMutex.TryLock() {
				boundary.requestMutex.Unlock()
				test.Error("another request can reach the gateway before the stream is consumed and closed")
			}
			if outcome == "cancelled" {
				cancel()
			}
			var readErr error
			if outcome != "early-close" {
				_, readErr = io.ReadAll(response.Body)
			}
			if outcome != "upgrade" && boundary.requestMutex.TryLock() {
				boundary.requestMutex.Unlock()
				test.Error("reading the stream released admission before body close")
			}
			closeErr := response.Body.Close()
			if err := response.Body.Close(); !errors.Is(err, closeErr) {
				test.Error("repeated body close changed its outcome")
			}
			if !boundary.requestMutex.TryLock() {
				test.Fatal("response close did not release request admission")
			}
			boundary.requestMutex.Unlock()
			if outcome == "complete" || outcome == "empty" {
				if readErr != nil || closeErr != nil || boundary.StopError() != nil {
					test.Fatalf("complete byte stream refused: read=%v close=%v stop=%v", readErr, closeErr, boundary.StopError())
				}
				followup, err := broker.RoundTrip(request)
				if err != nil {
					test.Fatal(err)
				}
				_, readErr := io.ReadAll(followup.Body)
				closeErr := followup.Body.Close()
				if readErr != nil || closeErr != nil || calls.Load() != 2 || boundary.StopError() != nil {
					test.Fatal("complete stream prevented the next permitted request")
				}
				return
			}
			if !errors.Is(boundary.StopError(), ErrOutcomeUnknown) || !errors.Is(readGatewayStop(boundary.state), ErrOutcomeUnknown) {
				test.Fatalf("incomplete stream did not persist an unknown outcome: %v", boundary.StopError())
			}
			if outcome == "truncated" || outcome == "cancelled" {
				if !errors.Is(readErr, ErrOutcomeUnknown) || strings.Contains(readErr.Error(), "unexpected EOF") || strings.Contains(readErr.Error(), "context canceled") {
					test.Errorf("body error was not replaced by the fixed unknown outcome: %v", readErr)
				}
			}
			select {
			case <-boundary.Stopped():
			default:
				test.Error("incomplete stream did not notify the native adapter")
			}
			for attempt := 0; attempt < 3; attempt++ {
				retry, err := http.NewRequest(http.MethodPost, upstream.URL+"/v1/responses", nil)
				if err != nil {
					test.Fatal(err)
				}
				denied, err := broker.RoundTrip(retry)
				if err != nil {
					test.Fatal(err)
				}
				contents, readErr := io.ReadAll(denied.Body)
				_ = denied.Body.Close()
				if readErr != nil || denied.StatusCode != http.StatusServiceUnavailable || string(contents) != ErrOutcomeUnknown.Error() || denied.Header.Get("Cache-Control") != "no-store" {
					test.Error("unknown-outcome refusal changed or leaked upstream state")
				}
			}
			if calls.Load() != 1 {
				test.Fatalf("incomplete stream allowed %d gateway calls", calls.Load())
			}
			if contents, err := os.ReadFile(marker); err != nil || string(contents) != "retained" {
				test.Error("unknown outcome reset native state")
			}
		})
	}
}

func TestCredentialGatewayStreamConcurrentClose(test *testing.T) {
	boundary := &Boundary{state: test.TempDir(), stopped: make(chan struct{})}
	reader, writer := io.Pipe()
	defer writer.Close()
	boundary.requestMutex.Lock()
	body := &gatewayResponseBody{body: reader, boundary: boundary}
	result := make(chan error, 1)
	go func() {
		_, err := body.Read(make([]byte, 1))
		result <- err
	}()
	var closes sync.WaitGroup
	for attempt := 0; attempt < 12; attempt++ {
		closes.Add(1)
		go func() {
			defer closes.Done()
			if err := body.Close(); !errors.Is(err, ErrOutcomeUnknown) {
				test.Errorf("concurrent close lost unknown outcome: %v", err)
			}
		}()
	}
	closes.Wait()
	select {
	case err := <-result:
		if !errors.Is(err, ErrOutcomeUnknown) {
			test.Errorf("close did not replace the blocked read error: %v", err)
		}
	case <-time.After(5 * time.Second):
		test.Fatal("close did not unblock the stream reader")
	}
	if !boundary.requestMutex.TryLock() {
		test.Fatal("concurrent close did not release request admission")
	}
	boundary.requestMutex.Unlock()
}

type failingGatewayClose struct{ io.Reader }

func (failingGatewayClose) Close() error { return errors.New("owned upstream secret") }

func TestCredentialGatewayStreamCloseFailure(test *testing.T) {
	boundary := &Boundary{state: test.TempDir(), stopped: make(chan struct{})}
	boundary.requestMutex.Lock()
	body := &gatewayResponseBody{body: failingGatewayClose{strings.NewReader("owned output")}, boundary: boundary}
	if _, err := io.ReadAll(body); err != nil {
		test.Fatal(err)
	}
	if err := body.Close(); !errors.Is(err, ErrOutcomeUnknown) || strings.Contains(err.Error(), "owned upstream secret") {
		test.Errorf("close failure leaked or lost its outcome: %v", err)
	}
	if !errors.Is(readGatewayStop(boundary.state), ErrOutcomeUnknown) {
		test.Error("close failure did not persist an unknown outcome")
	}
}
