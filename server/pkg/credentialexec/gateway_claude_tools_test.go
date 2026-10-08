package credentialexec

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const ownedClaudeToolBlock = `{"type":"tool_use","id":"tool_owned","name":"owned_tool","input":{"location":"owned"}}`

func ownedClaudeToolJSON() string {
	return strings.Replace(strings.Replace(completeClaudeJSON, `"content":[]`, `"content":[{"type":"text","text":"owned output"},`+ownedClaudeToolBlock+`]`, 1), `"end_turn"`, `"tool_use"`, 1)
}

func ownedClaudeToolEvents(index int, identity string, fragments ...string) string {
	blocks := ownedGatewayEvent("content_block_start", fmt.Sprintf(`"index":%d,"content_block":{"type":"tool_use","id":%q,"name":"owned_tool","input":{}}`, index, identity))
	for _, fragment := range fragments {
		contents, _ := json.Marshal(fragment)
		blocks += ownedGatewayEvent("content_block_delta", fmt.Sprintf(`"index":%d,"delta":{"type":"input_json_delta","partial_json":%s}`, index, contents))
	}
	return blocks + ownedGatewayEvent("content_block_stop", fmt.Sprintf(`"index":%d`, index))
}

func ownedClaudeToolStream(blocks string) string {
	return strings.Replace(strings.Replace(completeClaudeSSE, "event: message_delta", blocks+"event: message_delta", 1), `"end_turn"`, `"tool_use"`, 1)
}

func TestCredentialGatewayClaudeToolCompletion(test *testing.T) {
	for _, candidate := range []struct{ name, media, body string }{
		{"json", "application/json", ownedClaudeToolJSON()},
		{"stream", "text/event-stream", ownedClaudeToolStream(ownedClaudeToolEvents(0, "tool_owned", ``, `{"location":`, `"owned"}`))},
		{"empty-input", "text/event-stream", ownedClaudeToolStream(ownedClaudeToolEvents(0, "tool_owned"))},
		{"multiple-tools", "text/event-stream", ownedClaudeToolStream(ownedClaudeToolEvents(0, "tool_owned", `{}`) + ownedClaudeToolEvents(1, "tool_other", `{"count":2}`))},
	} {
		for _, chunkSize := range []int{1, 11, len(candidate.body)} {
			test.Run(fmt.Sprintf("%s-%d", candidate.name, chunkSize), func(test *testing.T) {
				request, _ := http.NewRequest(http.MethodPost, "http://owned/v1/messages", nil)
				protocol, err := newGatewayProtocol("claude", request, &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{candidate.media}}})
				if err != nil {
					test.Fatal(err)
				}
				var forwarded []byte
				for offset := 0; offset < len(candidate.body); offset += chunkSize {
					end := min(offset+chunkSize, len(candidate.body))
					if err := protocol.feed([]byte(candidate.body[offset:end])); err != nil {
						test.Fatal(err)
					}
					forwarded = append(forwarded, protocol.ready...)
					protocol.ready = nil
				}
				if err := protocol.finish(); err != nil {
					test.Fatal(err)
				}
				if protocol.stream && string(forwarded) != candidate.body {
					test.Error("tool stream wire bytes changed")
				}
				want := map[string]int64{"input_tokens": 7, "cache_creation_input_tokens": 2, "cache_read_input_tokens": 3, "output_tokens": 5}
				if !reflect.DeepEqual(protocol.usage, want) {
					test.Errorf("usage = %v, want %v", protocol.usage, want)
				}
				if protocol.toolBytes != 0 || len(protocol.toolInputs) != 0 {
					test.Error("closed tool inputs retained buffered arguments")
				}
			})
		}
	}
}

func TestCredentialGatewayClaudeToolRefusesMalformedOutcomes(test *testing.T) {
	stream := ownedClaudeToolStream(ownedClaudeToolEvents(0, "tool_owned", `{"location":"owned"}`))
	for _, candidate := range []struct{ name, media, body string }{
		{"missing-id", "application/json", strings.Replace(ownedClaudeToolJSON(), `"id":"tool_owned",`, "", 1)},
		{"missing-name", "application/json", strings.Replace(ownedClaudeToolJSON(), `"name":"owned_tool",`, "", 1)},
		{"null-input", "application/json", strings.Replace(ownedClaudeToolJSON(), `"input":{"location":"owned"}`, `"input":null`, 1)},
		{"array-input", "application/json", strings.Replace(ownedClaudeToolJSON(), `"input":{"location":"owned"}`, `"input":[]`, 1)},
		{"duplicate-input-field", "application/json", strings.Replace(ownedClaudeToolJSON(), `"location":"owned"`, `"location":"owned","LOCATION":"other"`, 1)},
		{"duplicate-id", "application/json", strings.Replace(ownedClaudeToolJSON(), ownedClaudeToolBlock, ownedClaudeToolBlock+","+ownedClaudeToolBlock, 1)},
		{"unsupported-server-tool", "application/json", strings.Replace(ownedClaudeToolJSON(), `"type":"tool_use"`, `"type":"server_tool_use"`, 1)},
		{"wrong-stop", "application/json", strings.Replace(ownedClaudeToolJSON(), `"stop_reason":"tool_use"`, `"stop_reason":"end_turn"`, 1)},
		{"missing-tool", "application/json", strings.Replace(completeClaudeJSON, `"end_turn"`, `"tool_use"`, 1)},
		{"nonempty-stream-start", "text/event-stream", strings.Replace(stream, `"input":{}`, `"input":{"location":"unverified"}`, 1)},
		{"wrong-delta", "text/event-stream", strings.Replace(stream, `"type":"input_json_delta"`, `"type":"text_delta"`, 1)},
		{"unfinished-input", "text/event-stream", ownedClaudeToolStream(ownedClaudeToolEvents(0, "tool_owned", `{"location":`))},
		{"array-stream-input", "text/event-stream", ownedClaudeToolStream(ownedClaudeToolEvents(0, "tool_owned", `[]`))},
		{"duplicate-stream-input", "text/event-stream", ownedClaudeToolStream(ownedClaudeToolEvents(0, "tool_owned", `{"location":1,"locati\u006fn":2}`))},
		{"deep-stream-input", "text/event-stream", ownedClaudeToolStream(ownedClaudeToolEvents(0, "tool_owned", `{"value":`+strings.Repeat("[", 33)+`0`+strings.Repeat("]", 33)+`}`))},
		{"duplicate-stream-id", "text/event-stream", ownedClaudeToolStream(ownedClaudeToolEvents(0, "tool_owned", `{}`) + ownedClaudeToolEvents(1, "tool_owned", `{}`))},
		{"unstopped-tool", "text/event-stream", strings.Replace(stream, ownedGatewayEvent("content_block_stop", `"index":0`), "", 1)},
		{"tool-with-text-stop", "text/event-stream", strings.Replace(stream, `"stop_reason":"tool_use"`, `"stop_reason":"end_turn"`, 1)},
		{"missing-stream-usage", "text/event-stream", strings.Replace(stream, `"output_tokens":5`, `"output_tokens":null`, 1)},
		{"input-buffer-limit", "text/event-stream", ownedClaudeToolStream(ownedClaudeToolEvents(0, "tool_owned", `{"value":"`, strings.Repeat("x", gatewayFrameLimit/2), strings.Repeat("x", gatewayFrameLimit/2), `"}`))},
		{"aggregate-input-buffer-limit", "text/event-stream", ownedClaudeToolStream(strings.Replace(ownedClaudeToolEvents(0, "tool_owned", `{"value":"`+strings.Repeat("x", gatewayFrameLimit/2)+`"}`), ownedGatewayEvent("content_block_stop", `"index":0`), "", 1) + ownedClaudeToolEvents(1, "tool_other", `{"value":"`+strings.Repeat("x", gatewayFrameLimit/2)+`"}`))},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			request, _ := http.NewRequest(http.MethodPost, "http://owned/v1/messages", nil)
			protocol, err := newGatewayProtocol("claude", request, &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{candidate.media}}})
			if err != nil {
				test.Fatal(err)
			}
			if protocol.feed([]byte(candidate.body)) == nil && protocol.finish() == nil {
				test.Fatal("malformed tool outcome accepted")
			}
		})
	}
}
