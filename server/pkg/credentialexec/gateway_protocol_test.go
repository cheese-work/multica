package credentialexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const completeClaudeJSON = `{"id":"msg_owned","type":"message","role":"assistant","model":"owned-model","content":[],"stop_reason":"end_turn","usage":{"input_tokens":7,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":5}}`
const completeResponsesJSON = `{"id":"resp_owned","object":"response","model":"owned-model","status":"completed","output":[],"usage":{"input_tokens":12,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":2},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":17}}`
const completeClaudeSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_owned\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"owned-model\",\"content\":[],\"usage\":{\"input_tokens\":7,\"cache_creation_input_tokens\":2,\"cache_read_input_tokens\":3,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
const completeResponsesSSE = "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_owned\",\"object\":\"response\",\"model\":\"owned-model\",\"status\":\"in_progress\"}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":" + completeResponsesJSON + "}\n\n"

func TestCredentialGatewayProtocolCompletion(test *testing.T) {
	cases := []struct {
		name, provider, contentType, body string
		complete                          bool
	}{
		{"claude-json", "claude", "application/json", completeClaudeJSON, true},
		{"responses-json", "codex", "application/json", completeResponsesJSON, true},
		{"claude-stream", "claude", "text/event-stream", completeClaudeSSE, true},
		{"claude-tool-json", "claude", "application/json", ownedClaudeToolJSON(), true},
		{"claude-tool-stream", "claude", "text/event-stream", ownedClaudeToolStream(ownedClaudeToolEvents(0, "tool_owned", `{"location":"owned"}`)), true},
		{"claude-tool-incomplete", "claude", "text/event-stream", ownedClaudeToolStream(ownedClaudeToolEvents(0, "tool_owned", `{"location":`)), false},
		{"claude-tool-usage-missing", "claude", "text/event-stream", strings.Replace(ownedClaudeToolStream(ownedClaudeToolEvents(0, "tool_owned", `{}`)), `"output_tokens":5`, `"output_tokens":null`, 1), false},
		{"responses-stream", "codex", "text/event-stream", completeResponsesSSE, true},
		{"responses-terminal-missing", "codex", "text/event-stream", strings.Split(completeResponsesSSE, "event: response.completed")[0], false},
		{"claude-stop-missing", "claude", "text/event-stream", strings.Split(completeClaudeSSE, "event: message_stop")[0], false},
		{"claude-usage-missing", "claude", "text/event-stream", strings.Replace(completeClaudeSSE, `"output_tokens":5`, `"output_tokens":null`, 1), false},
		{"responses-usage-missing", "codex", "application/json", strings.Replace(completeResponsesJSON, `"output_tokens":5`, `"output_tokens":null`, 1), false},
		{"responses-incomplete", "codex", "application/json", strings.Replace(completeResponsesJSON, `"status":"completed"`, `"status":"incomplete"`, 1), false},
		{"responses-negative-usage", "codex", "application/json", strings.Replace(completeResponsesJSON, `"input_tokens":12`, `"input_tokens":-12`, 1), false},
		{"responses-inconsistent-total", "codex", "application/json", strings.Replace(completeResponsesJSON, `"total_tokens":17`, `"total_tokens":18`, 1), false},
		{"responses-unknown-cache", "codex", "application/json", strings.Replace(completeResponsesJSON, `,"cache_write_tokens":2`, "", 1), false},
		{"duplicate-json-field", "codex", "application/json", strings.Replace(completeResponsesJSON, `"output_tokens":5`, `"output_tokens":null,"output_tokens":5`, 1), false},
		{"stream-after-terminal", "codex", "text/event-stream", completeResponsesSSE + completeResponsesSSE, false},
		{"stream-event-type-mismatch", "codex", "text/event-stream", strings.Replace(completeResponsesSSE, "event: response.completed", "event: response.failed", 1), false},
		{"stream-error-after-completion", "claude", "text/event-stream", completeClaudeSSE + "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"owned upstream secret\"}}\n\n", false},
		{"empty-success", "codex", "application/json", "", false},
		{"opaque-success", "codex", "text/plain", "owned upstream secret", false},
	}
	for _, candidate := range cases {
		test.Run(candidate.name, func(test *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				writer.Header().Set("Content-Type", candidate.contentType)
				_, _ = io.WriteString(writer, candidate.body)
			}))
			defer upstream.Close()
			boundary := &Boundary{state: test.TempDir(), spec: Spec{Provider: candidate.provider}, stopped: make(chan struct{})}
			if err := os.WriteFile(filepath.Join(boundary.state, "native-state"), []byte("retained"), 0600); err != nil {
				test.Fatal(err)
			}
			transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
			defer transport.CloseIdleConnections()
			broker := &gatewayTransport{boundary: boundary, transport: transport}
			path := "/v1/responses"
			if candidate.provider == "claude" {
				path = "/v1/messages"
			}
			request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, upstream.URL+path, nil)
			if err != nil {
				test.Fatal(err)
			}
			response, err := broker.RoundTrip(request)
			if err != nil {
				test.Fatal(err)
			}
			contents, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if candidate.complete {
				if readErr != nil || closeErr != nil || boundary.StopError() != nil || string(contents) != candidate.body {
					test.Fatalf("complete provider response changed/refused: read=%v close=%v stop=%v", readErr, closeErr, boundary.StopError())
				}
			} else if !errors.Is(boundary.StopError(), ErrOutcomeUnknown) {
				test.Errorf("clean HTTP EOF accepted incomplete/unknown protocol outcome: read=%v close=%v stop=%v", readErr, closeErr, boundary.StopError())
			}
			if !candidate.complete && strings.Contains(string(contents), "owned upstream secret") {
				test.Error("refused protocol echoed upstream error payload")
			}
			followup, err := broker.RoundTrip(request)
			if err != nil {
				test.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, followup.Body)
			_ = followup.Body.Close()
			if candidate.complete && calls.Load() != 2 || !candidate.complete && calls.Load() != 1 {
				test.Errorf("provider completion admission allowed %d gateway calls", calls.Load())
			}
			if !candidate.complete && !errors.Is(readGatewayStop(boundary.state), ErrOutcomeUnknown) {
				test.Error("semantic refusal did not persist same-task unknown outcome")
			}
		})
	}
}

func ownedGatewayEvent(eventType, fields string) string {
	return "event: " + eventType + "\ndata: {\"type\":\"" + eventType + "\"," + fields + "}\n\n"
}

func ownedGatewayTextStreams() (string, string) {
	claude := strings.Replace(completeClaudeSSE, "event: message_delta", ownedGatewayEvent("content_block_start", `"index":0,"content_block":{"type":"text","text":""}`)+ownedGatewayEvent("content_block_delta", `"index":0,"delta":{"type":"text_delta","text":"owned output"}`)+ownedGatewayEvent("content_block_stop", `"index":0`)+"event: message_delta", 1)
	start := strings.Split(completeResponsesSSE, "event: response.completed")[0]
	item := `{"id":"item_owned","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"owned output","annotations":[]}]}`
	responses := start + ownedGatewayEvent("response.output_item.added", `"sequence_number":1,"output_index":0,"item":{"id":"item_owned","type":"message","role":"assistant","status":"in_progress","content":[]}`)
	responses += ownedGatewayEvent("response.content_part.added", `"sequence_number":2,"output_index":0,"content_index":0,"item_id":"item_owned","part":{"type":"output_text","text":"","annotations":[]}`)
	responses += ownedGatewayEvent("response.output_text.delta", `"sequence_number":3,"output_index":0,"content_index":0,"item_id":"item_owned","delta":"owned output"`)
	responses += ownedGatewayEvent("response.output_text.done", `"sequence_number":4,"output_index":0,"content_index":0,"item_id":"item_owned","text":"owned output"`)
	responses += ownedGatewayEvent("response.content_part.done", `"sequence_number":5,"output_index":0,"content_index":0,"item_id":"item_owned","part":{"type":"output_text","text":"owned output","annotations":[]}`)
	responses += ownedGatewayEvent("response.output_item.done", `"sequence_number":6,"output_index":0,"item":`+item)
	responses += ownedGatewayEvent("response.completed", `"sequence_number":7,"response":`+strings.Replace(completeResponsesJSON, `"output":[]`, `"output":[`+item+`]`, 1))
	return claude, responses
}

func TestCredentialGatewayProtocolFragmentationAndUsage(test *testing.T) {
	claudeText, responsesText := ownedGatewayTextStreams()
	cases := []struct{ provider, media, body string }{
		{"claude", "application/json", completeClaudeJSON},
		{"codex", "application/json", completeResponsesJSON},
		{"claude", "text/event-stream", claudeText},
		{"codex", "text/event-stream", responsesText},
		{"claude", "text/event-stream", strings.ReplaceAll(claudeText, "\n", "\r\n")},
		{"claude", "text/event-stream", strings.Replace(claudeText, `"message":{`, "\"message\":\ndata: {", 1)},
		{"claude", "text/event-stream", strings.Replace(claudeText, `"output_tokens":5`, `"output_tokens":5,"input_tokens":7,"cache_creation_input_tokens":2,"cache_read_input_tokens":3`, 1)},
	}
	for index, candidate := range cases {
		for _, size := range []int{1, 7, 4096} {
			test.Run(fmt.Sprintf("case=%d/chunk=%d", index, size), func(test *testing.T) {
				path := "/v1/responses"
				if candidate.provider == "claude" {
					path = "/v1/messages"
				}
				request, _ := http.NewRequest(http.MethodPost, "http://owned"+path, nil)
				protocol, err := newGatewayProtocol(candidate.provider, request, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{candidate.media}}})
				if err != nil {
					test.Fatal(err)
				}
				var forwarded []byte
				for offset := 0; offset < len(candidate.body); offset += size {
					end := min(offset+size, len(candidate.body))
					if err := protocol.feed([]byte(candidate.body[offset:end])); err != nil {
						test.Fatalf("fragment at %d: %v", offset, err)
					}
					forwarded = append(forwarded, protocol.ready...)
					protocol.ready = nil
				}
				if err := protocol.finish(); err != nil {
					test.Fatal(err)
				}
				if protocol.stream && string(forwarded) != candidate.body {
					test.Error("valid streamed wire bytes changed")
				}
				want := map[string]int64{"input_tokens": 7, "cache_creation_input_tokens": 2, "cache_read_input_tokens": 3, "output_tokens": 5}
				if !reflect.DeepEqual(protocol.usage, want) {
					test.Errorf("usage double-counted or lost: got %v want %v", protocol.usage, want)
				}
			})
		}
	}
}

func TestCredentialGatewayProtocolRefusesMalformedOutcomes(test *testing.T) {
	claudeText, responsesText := ownedGatewayTextStreams()
	cases := []struct{ name, provider, media, body string }{
		{"fraction", "codex", "application/json", strings.Replace(completeResponsesJSON, `"input_tokens":12`, `"input_tokens":12.0`, 1)},
		{"exponent", "codex", "application/json", strings.Replace(completeResponsesJSON, `"input_tokens":12`, `"input_tokens":12e0`, 1)},
		{"overflow", "codex", "application/json", strings.Replace(completeResponsesJSON, `"total_tokens":17`, `"total_tokens":9223372036854775808`, 1)},
		{"cache-overlap", "codex", "application/json", strings.Replace(completeResponsesJSON, `"cache_write_tokens":2`, `"cache_write_tokens":10`, 1)},
		{"reasoning-overlap", "codex", "application/json", strings.Replace(completeResponsesJSON, `"reasoning_tokens":2`, `"reasoning_tokens":6`, 1)},
		{"case-duplicate", "codex", "application/json", strings.Replace(completeResponsesJSON, `"output_tokens":5`, `"output_tokens":5,"OUTPUT_TOKENS":5`, 1)},
		{"escaped-duplicate", "codex", "application/json", strings.Replace(completeResponsesJSON, `"output_tokens":5`, `"output_tokens":5,"output_\u0074okens":5`, 1)},
		{"claude-missing-input", "claude", "application/json", strings.Replace(completeClaudeJSON, `"input_tokens":7,`, "", 1)},
		{"claude-server-tool", "claude", "application/json", strings.Replace(completeClaudeJSON, `"output_tokens":5`, `"output_tokens":5,"server_tool_use":{"web_search_requests":1}`, 1)},
		{"claude-unknown-block", "claude", "text/event-stream", strings.Replace(claudeText, `"type":"text","text":""`, `"type":"server_tool_use","text":""`, 1)},
		{"claude-unknown-delta", "claude", "text/event-stream", strings.Replace(claudeText, `"type":"text_delta"`, `"type":"input_json_delta"`, 1)},
		{"claude-unstopped-block", "claude", "text/event-stream", strings.Replace(claudeText, ownedGatewayEvent("content_block_stop", `"index":0`), "", 1)},
		{"claude-decreasing-usage", "claude", "text/event-stream", strings.Replace(claudeText, `"output_tokens":5`, `"output_tokens":5,"input_tokens":6`, 1)},
		{"responses-changed-id", "codex", "text/event-stream", strings.Replace(completeResponsesSSE, `"id":"resp_owned"`, `"id":"changed"`, 1)},
		{"responses-unknown-item", "codex", "text/event-stream", strings.Replace(responsesText, `"type":"message"`, `"type":"computer_call"`, 1)},
		{"responses-missing-text-done", "codex", "text/event-stream", strings.Replace(responsesText, "response.output_text.done", "response.output_text.delta", 2)},
		{"responses-changed-item-id", "codex", "text/event-stream", strings.Replace(responsesText, `"item_id":"item_owned"`, `"item_id":"other"`, 1)},
		{"responses-unfinished-item", "codex", "text/event-stream", strings.Replace(responsesText, "response.output_item.done", "response.in_progress", 2)},
		{"responses-sequence", "codex", "text/event-stream", strings.Replace(responsesText, `"sequence_number":3`, `"sequence_number":4`, 1)},
		{"responses-text-mismatch", "codex", "text/event-stream", strings.Replace(responsesText, `"delta":"owned output"`, `"delta":"changed output"`, 1)},
		{"partial-frame", "codex", "text/event-stream", strings.TrimSuffix(completeResponsesSSE, "\n")},
		{"trailing-error", "codex", "application/json", completeResponsesJSON + `{"error":"owned secret"}`},
		{"overlong-frame", "claude", "text/event-stream", ":" + strings.Repeat("x", gatewayFrameLimit) + "\n\n"},
		{"overlong-json", "claude", "application/json", `{"text":"` + strings.Repeat("x", gatewayFrameLimit) + `"}`},
		{"deep-json", "claude", "application/json", `{"nested":` + strings.Repeat("[", 33) + `0` + strings.Repeat("]", 33) + `}`},
		{"invalid-utf8", "claude", "text/event-stream", "data: \xff\n\n"},
		{"unsupported-sse-field", "codex", "text/event-stream", "retry: 1\n" + completeResponsesSSE},
	}
	for _, candidate := range cases {
		test.Run(candidate.name, func(test *testing.T) {
			path := "/v1/responses"
			if candidate.provider == "claude" {
				path = "/v1/messages"
			}
			request, _ := http.NewRequest(http.MethodPost, "http://owned"+path, nil)
			protocol, err := newGatewayProtocol(candidate.provider, request, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{candidate.media}}})
			if err != nil {
				test.Fatal(err)
			}
			if protocol.feed([]byte(candidate.body)) == nil && protocol.finish() == nil {
				test.Fatal("malformed provider outcome accepted")
			}
		})
	}
}

func TestCredentialGatewayProtocolBufferedCloseAndErrorRedaction(test *testing.T) {
	for _, outcome := range []string{"unread-buffer", "split-error", "late-trailer"} {
		test.Run(outcome, func(test *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				if outcome == "split-error" {
					writer.Header().Set("Content-Type", "text/event-stream")
					for _, character := range "event: error\ndata: {\"type\":\"error\",\"message\":\"owned upstream secret\"}\n\n" {
						_, _ = fmt.Fprintf(writer, "%c", character)
						writer.(http.Flusher).Flush()
					}
					return
				}
				_, _ = io.WriteString(writer, completeResponsesJSON)
				if outcome == "late-trailer" {
					writer.(http.Flusher).Flush()
					writer.Header().Set(http.TrailerPrefix+"Owned", "owned upstream secret")
				}
			}))
			defer upstream.Close()
			boundary := &Boundary{state: test.TempDir(), spec: Spec{Provider: "codex"}, stopped: make(chan struct{})}
			transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
			defer transport.CloseIdleConnections()
			request, _ := http.NewRequest(http.MethodPost, upstream.URL+"/v1/responses", nil)
			response, err := (&gatewayTransport{boundary: boundary, transport: transport}).RoundTrip(request)
			if err != nil {
				test.Fatal(err)
			}
			if outcome == "unread-buffer" {
				if _, err := response.Body.Read(make([]byte, 1)); err != nil {
					test.Fatal(err)
				}
			} else {
				contents, _ := io.ReadAll(response.Body)
				if strings.Contains(string(contents), "owned upstream secret") {
					test.Fatal("refused frame echoed")
				}
			}
			_ = response.Body.Close()
			if !errors.Is(boundary.StopError(), ErrOutcomeUnknown) {
				test.Fatal("unread/invalid outcome admitted", boundary.StopError())
			}
		})
	}
}

func TestCredentialGatewayProtocolZeroAndMetadata(test *testing.T) {
	var object map[string]any
	if err := json.Unmarshal([]byte(completeResponsesJSON), &object); err != nil {
		test.Fatal(err)
	}
	object["usage"] = map[string]any{"input_tokens": 0, "input_tokens_details": map[string]any{"cached_tokens": 0, "cache_write_tokens": 0}, "output_tokens": 0, "output_tokens_details": map[string]any{"reasoning_tokens": 0}, "total_tokens": 0}
	contents, _ := json.Marshal(object)
	request, _ := http.NewRequest(http.MethodPost, "http://owned/v1/responses", nil)
	response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}}
	protocol, err := newGatewayProtocol("codex", request, response)
	if err != nil || protocol.feed(contents) != nil || protocol.finish() != nil {
		test.Fatal("explicit zero usage refused", err)
	}
	for _, count := range protocol.usage {
		if count != 0 {
			test.Fatal("explicit zero changed")
		}
	}
	for _, metadata := range []struct{ method, path, body, provider string }{
		{http.MethodGet, "/v1/models", `{"data":[]}`, "codex"},
		{http.MethodPost, "/v1/messages/count_tokens", `{"input_tokens":0}`, "claude"},
	} {
		metadataRequest, _ := http.NewRequest(metadata.method, "http://owned"+metadata.path, nil)
		metadataProtocol, err := newGatewayProtocol(metadata.provider, metadataRequest, response)
		if err != nil || metadataProtocol.feed([]byte(metadata.body)) != nil || metadataProtocol.finish() != nil {
			test.Fatal("bounded metadata refused", err)
		}
		if len(metadataProtocol.usage) != 0 {
			test.Fatal("metadata invented inference usage")
		}
	}
	response.Trailer = http.Header{"Owned-Trailer": nil}
	if _, err := newGatewayProtocol("codex", request, response); err == nil {
		test.Fatal("declared trailer accepted")
	}
	request.URL.Path = "/v1/chat/completions"
	if allowedGatewayRequest("codex", request) {
		test.Fatal("unsupported opted-in chat completion forwarded")
	}
}

func TestCredentialGatewayProtocolStreamsBeforeEOF(test *testing.T) {
	prefix, suffix, _ := strings.Cut(completeClaudeSSE, "event: message_delta")
	finish := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, prefix)
		writer.(http.Flusher).Flush()
		<-finish
		_, _ = io.WriteString(writer, "event: message_delta"+suffix)
	}))
	defer upstream.Close()
	defer close(finish)
	boundary := &Boundary{state: test.TempDir(), spec: Spec{Provider: "claude"}, stopped: make(chan struct{})}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+"/v1/messages", nil)
	response, err := (&gatewayTransport{boundary: boundary, transport: transport}).RoundTrip(request)
	if err != nil {
		test.Fatal(err)
	}
	defer response.Body.Close()
	buffer := make([]byte, len(prefix))
	if _, err := io.ReadFull(response.Body, buffer); err != nil || string(buffer) != prefix {
		test.Fatal("validated frame withheld until EOF", err)
	}
	if boundary.requestMutex.TryLock() {
		boundary.requestMutex.Unlock()
		test.Fatal("stream frame released admission before completion")
	}
	finish <- struct{}{}
	contents, err := io.ReadAll(response.Body)
	if err != nil || string(contents) != "event: message_delta"+suffix || response.Body.Close() != nil || boundary.StopError() != nil {
		test.Fatal("valid stream completion refused", err)
	}
}
