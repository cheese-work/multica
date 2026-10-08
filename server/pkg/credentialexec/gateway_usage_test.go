package credentialexec

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestCredentialGatewayUsageCommitsAfterClose(test *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, stream := range []bool{false, true} {
			name := provider + "/json"
			if stream {
				name = provider + "/sse"
			}
			test.Run(name, func(test *testing.T) {
				contents, path := completeClaudeJSON, "/v1/messages"
				if provider == "codex" {
					contents, path = completeResponsesJSON, "/v1/responses"
				}
				mediaType := "application/json"
				if stream {
					mediaType, contents = "text/event-stream", completeClaudeSSE
					if provider == "codex" {
						contents = completeResponsesSSE
					}
				}
				upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
					writer.Header().Set("Content-Type", mediaType)
					_, _ = io.WriteString(writer, contents)
				}))
				defer upstream.Close()
				boundary := &Boundary{state: test.TempDir(), spec: Spec{Provider: provider}, stopped: make(chan struct{})}
				transport := &gatewayTransport{boundary: boundary, transport: &http.Transport{Proxy: nil}}
				defer transport.transport.CloseIdleConnections()
				if snapshot := boundary.UsageSnapshot(); snapshot.Complete || snapshot.Models != nil {
					test.Fatal("no inference became known zero usage")
				}
				for attempt := 0; attempt < 2; attempt++ {
					request, err := http.NewRequest(http.MethodPost, upstream.URL+path, nil)
					if err != nil {
						test.Fatal(err)
					}
					response, err := transport.RoundTrip(request)
					if err != nil {
						test.Fatal(err)
					}
					if boundary.UsageSnapshot().Complete {
						test.Fatal("in-flight inference claimed complete usage")
					}
					if _, err := io.ReadAll(response.Body); err != nil {
						test.Fatal(err)
					}
					if boundary.UsageSnapshot().Complete {
						test.Fatal("byte EOF committed usage before successful close")
					}
					for closeAttempt := 0; closeAttempt < 2; closeAttempt++ {
						if err := response.Body.Close(); err != nil {
							test.Fatal("valid response close failed")
						}
					}
					expected := GatewayUsage{InputTokens: 7 * int64(attempt+1), OutputTokens: 5 * int64(attempt+1), CacheReadTokens: 3 * int64(attempt+1), CacheWriteTokens: 2 * int64(attempt+1)}
					snapshot := boundary.UsageSnapshot()
					if !snapshot.Complete || len(snapshot.Models) != 1 || snapshot.Models["owned-model"] != expected {
						test.Fatalf("trusted disjoint usage was lost or double-counted: %+v", snapshot)
					}
					snapshot.Models["owned-model"] = GatewayUsage{}
					if boundary.UsageSnapshot().Models["owned-model"] != expected {
						test.Fatal("snapshot exposed mutable accounting state")
					}
				}
			})
		}
	}
}

func TestCredentialGatewayUsageUnknownPreservesObserved(test *testing.T) {
	for _, outcome := range []string{"quota", "missing", "early-close", "metadata"} {
		test.Run(outcome, func(test *testing.T) {
			var calls int
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				calls++
				writer.Header().Set("Content-Type", "application/json")
				if calls == 1 {
					_, _ = io.WriteString(writer, completeResponsesJSON)
					return
				}
				switch outcome {
				case "quota":
					writer.WriteHeader(http.StatusTooManyRequests)
				case "missing":
					_, _ = io.WriteString(writer, strings.Replace(completeResponsesJSON, `"total_tokens":17`, `"total_tokens":null`, 1))
				case "metadata":
					_, _ = io.WriteString(writer, `{"data":[]}`)
				default:
					_, _ = io.WriteString(writer, completeResponsesJSON)
				}
			}))
			defer upstream.Close()
			boundary := &Boundary{state: test.TempDir(), spec: Spec{Provider: "codex"}, stopped: make(chan struct{})}
			transport := &gatewayTransport{boundary: boundary, transport: &http.Transport{Proxy: nil}}
			defer transport.transport.CloseIdleConnections()
			request, _ := http.NewRequest(http.MethodPost, upstream.URL+"/v1/responses", nil)
			response, err := transport.RoundTrip(request)
			if err != nil {
				test.Fatal(err)
			}
			if _, err := io.ReadAll(response.Body); err != nil || response.Body.Close() != nil {
				test.Fatal("owned complete response failed")
			}
			before := boundary.UsageSnapshot()
			if !before.Complete {
				test.Fatal("complete response was not observed")
			}
			if outcome == "metadata" {
				request, _ = http.NewRequest(http.MethodGet, upstream.URL+"/v1/models", nil)
			}
			response, err = transport.RoundTrip(request)
			if err != nil {
				test.Fatal(err)
			}
			if outcome != "early-close" {
				_, _ = io.ReadAll(response.Body)
			}
			_ = response.Body.Close()
			after := boundary.UsageSnapshot()
			if !reflect.DeepEqual(before.Models, after.Models) || after.Complete != (outcome == "metadata") {
				test.Fatalf("unknown outcome reset/committed usage or metadata counted as inference: %+v", after)
			}
		})
	}
}

func TestCredentialGatewayUsageOverflowRefusesAtomically(test *testing.T) {
	boundary := &Boundary{state: test.TempDir(), spec: Spec{Provider: "codex"}, stopped: make(chan struct{})}
	boundary.usage = map[string]GatewayUsage{"owned-model": {InputTokens: math.MaxInt64 - 10, OutputTokens: 5}}
	before := boundary.UsageSnapshot()
	protocol := &gatewayProtocol{model: "owned-model", responseID: "owned-response", provider: "codex", path: "/v1/responses", usage: map[string]int64{"input_tokens": 7, "output_tokens": 5, "cache_read_input_tokens": 3, "cache_creation_input_tokens": 2}}
	if err := boundary.recordGatewayUsage(protocol); !errors.Is(err, ErrOutcomeUnknown) {
		test.Fatal("overflowing usage aggregate was admitted")
	}
	if !reflect.DeepEqual(before.Models, boundary.UsageSnapshot().Models) {
		test.Fatal("overflow partially changed known counters")
	}
}

func TestCredentialGatewayUsageZeroAndMetadata(test *testing.T) {
	for _, path := range []string{"/v1/models", "/v1/messages/count_tokens", "/v1/messages", "/v1/responses"} {
		test.Run(path, func(test *testing.T) {
			boundary := &Boundary{state: test.TempDir(), stopped: make(chan struct{})}
			protocol := &gatewayProtocol{path: path, model: "owned-model", responseID: "owned-response", usage: map[string]int64{"input_tokens": 0, "output_tokens": 0, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}}
			if err := boundary.recordGatewayUsage(protocol); err != nil {
				test.Fatal(err)
			}
			snapshot := boundary.UsageSnapshot()
			inference := path == "/v1/messages" || path == "/v1/responses"
			if snapshot.Complete != inference || inference && (len(snapshot.Models) != 1 || snapshot.Models["owned-model"] != (GatewayUsage{})) || !inference && snapshot.Models != nil {
				test.Fatalf("explicit zero and unknown metadata usage were conflated: %+v", snapshot)
			}
		})
	}
}

func TestCredentialGatewayUsageCloseFailure(test *testing.T) {
	boundary := &Boundary{state: test.TempDir(), spec: Spec{Provider: "codex"}, stopped: make(chan struct{})}
	request, _ := http.NewRequest(http.MethodPost, "http://owned/v1/responses", nil)
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}}
	protocol, err := newGatewayProtocol("codex", request, response)
	if err != nil {
		test.Fatal(err)
	}
	boundary.requestMutex.Lock()
	body := &gatewayResponseBody{body: failingGatewayClose{strings.NewReader(completeResponsesJSON)}, boundary: boundary, protocol: protocol, response: response}
	if _, err := io.ReadAll(body); err != nil {
		test.Fatal(err)
	}
	if err := body.Close(); !errors.Is(err, ErrOutcomeUnknown) {
		test.Fatal("failed close admitted final usage")
	}
	if snapshot := boundary.UsageSnapshot(); snapshot.Complete || snapshot.Models != nil {
		test.Fatal("failed close committed inferred usage")
	}
}

func TestCredentialGatewayUsageBounds(test *testing.T) {
	boundary := &Boundary{state: test.TempDir(), stopped: make(chan struct{}), usage: make(map[string]GatewayUsage)}
	protocol := &gatewayProtocol{path: "/v1/responses", model: "owned-model", responseID: "owned-response", usage: map[string]int64{"input_tokens": 0, "output_tokens": 0, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}}
	for model := 0; model < 128; model++ {
		protocol.model = fmt.Sprintf("owned-model-%d", model)
		if err := boundary.recordGatewayUsage(protocol); err != nil {
			test.Fatal(err)
		}
	}
	before := boundary.UsageSnapshot()
	protocol.model = "overflow-model"
	if err := boundary.recordGatewayUsage(protocol); !errors.Is(err, ErrOutcomeUnknown) {
		test.Fatal("unbounded per-model usage admitted")
	}
	protocol.model = strings.Repeat("a", 1025)
	if err := boundary.recordGatewayUsage(protocol); !errors.Is(err, ErrOutcomeUnknown) {
		test.Fatal("oversized model identity admitted")
	}
	protocol.model = "owned-model-0"
	delete(protocol.usage, "input_tokens")
	if err := boundary.recordGatewayUsage(protocol); !errors.Is(err, ErrOutcomeUnknown) {
		test.Fatal("missing counter became known zero")
	}
	if !reflect.DeepEqual(before.Models, boundary.UsageSnapshot().Models) {
		test.Fatal("rejected counters partially changed observed usage")
	}
}
