package credentialexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCredentialGatewayStopIsTerminal(test *testing.T) {
	if _, err := os.Stat("/usr/bin/bwrap"); os.IsNotExist(err) {
		test.Skip("gateway stop namespace integration NOT-RUN: system bubblewrap unavailable")
	}
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway, 0} {
		test.Run(http.StatusText(status), func(test *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				if status == 0 {
					connection, _, err := writer.(http.Hijacker).Hijack()
					if err != nil {
						test.Error(err)
						return
					}
					_ = connection.Close()
					return
				}
				writer.WriteHeader(status)
				_, _ = io.WriteString(writer, "owned upstream secret must not be a result")
			}))
			defer upstream.Close()
			helper, err := os.Executable()
			if err != nil {
				test.Fatal(err)
			}
			contents, err := os.ReadFile(helper)
			if err != nil {
				test.Fatal(err)
			}
			executable := filepath.Join(test.TempDir(), "owned-native")
			if err := os.WriteFile(executable, contents, 0500); err != nil {
				test.Fatal(err)
			}
			spec := Spec{Root: filepath.Join(test.TempDir(), "private"), Binding: Binding{TaskID: "00000000-0000-4000-8000-000000000001", OwnerID: "00000000-0000-4000-8000-000000000002", WorkspaceID: "00000000-0000-4000-8000-000000000003"}, Provider: "codex", Executable: executable, HelperExecutable: helper}
			boundary, err := Prepare(context.Background(), spec)
			if err != nil {
				test.Fatal(err)
			}
			defer boundary.Close()
			if err := os.WriteFile(filepath.Join(boundary.Home(), "native-state"), []byte("retained"), 0600); err != nil {
				test.Fatal(err)
			}
			if err := boundary.BindGateway(context.Background(), GatewayCredential{Binding: spec.Binding, BaseURL: upstream.URL, Key: "owned-task-key"}); err != nil {
				test.Fatal(err)
			}
			transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", boundary.socket)
			}}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport}
			expected := ErrOutcomeUnknown
			expectedStatus := http.StatusServiceUnavailable
			if status == http.StatusTooManyRequests {
				expected = ErrQuotaExhausted
				expectedStatus = http.StatusTooManyRequests
			}
			var requests sync.WaitGroup
			for attempt := 0; attempt < 12; attempt++ {
				requests.Add(1)
				go func() {
					defer requests.Done()
					response, err := client.Post("http://owned-broker/v1/responses", "application/json", nil)
					if err != nil {
						test.Error(err)
						return
					}
					defer response.Body.Close()
					body, err := io.ReadAll(response.Body)
					if err != nil {
						test.Error(err)
					} else if response.StatusCode != expectedStatus || string(body) != expected.Error() || response.Header.Get("Cache-Control") != "no-store" {
						test.Error("terminal response leaked upstream state or lost authoritative failure")
					}
				}()
			}
			requests.Wait()
			if calls.Load() != 1 || !errors.Is(boundary.StopError(), expected) {
				test.Fatal("gateway refusal was retried or its authoritative outcome lost")
			}
			select {
			case <-boundary.Stopped():
			default:
				test.Fatal("adapter stop notification missing")
			}
			if err := boundary.Validate(spec.Binding.TaskID, spec.Provider, spec.Executable); !errors.Is(err, expected) {
				test.Fatal("stopped boundary admits another launch")
			}
			if err := boundary.Close(); err != nil {
				test.Fatal(err)
			}
			if resumed, err := Prepare(context.Background(), spec); !errors.Is(err, expected) || resumed != nil {
				test.Fatal("same-task preparation cleared a terminal gateway refusal")
			}
			if state, err := os.ReadFile(filepath.Join(boundary.Home(), "native-state")); err != nil || string(state) != "retained" {
				test.Fatal("stop reset native state")
			}
		})
	}
}

func TestCredentialGatewayStopMarkerRefusesInvalidState(test *testing.T) {
	for _, contents := range []string{"", "unknown\n", "quota\n", "quota", strings.Repeat("x", 17)} {
		test.Run(fmt.Sprintf("contents=%q", contents), func(test *testing.T) {
			state := test.TempDir()
			if err := readGatewayStop(state); err != nil {
				test.Fatal("absent marker refused fresh state")
			}
			if err := os.WriteFile(filepath.Join(state, "gateway-stop"), []byte(contents), 0600); err != nil {
				test.Fatal(err)
			}
			expected := ErrUnavailable
			if contents == "quota\n" {
				expected = ErrQuotaExhausted
			} else if contents == "unknown\n" {
				expected = ErrOutcomeUnknown
			}
			boundary := &Boundary{state: state, stopped: make(chan struct{})}
			for attempt := 0; attempt < 2; attempt++ {
				if err := boundary.StopError(); !errors.Is(err, expected) {
					test.Fatalf("marker failed open: %v", err)
				}
				select {
				case <-boundary.Stopped():
				default:
					test.Fatal("persisted refusal did not notify launch")
				}
			}
		})
	}
	for _, kind := range []string{"public", "symlink", "directory"} {
		test.Run(kind, func(test *testing.T) {
			state := test.TempDir()
			path := filepath.Join(state, "gateway-stop")
			var err error
			switch kind {
			case "public":
				err = os.WriteFile(path, []byte("quota\n"), 0644)
			case "symlink":
				err = os.Symlink(filepath.Join(state, "absent"), path)
			case "directory":
				err = os.Mkdir(path, 0700)
			}
			if err != nil {
				test.Fatal(err)
			}
			if err := readGatewayStop(state); !errors.Is(err, ErrUnavailable) {
				test.Fatal("unsafe marker admitted")
			}
		})
	}
}
