package credentialexec

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const ownedOpenAIToolItem = `{"id":"item_owned","type":"function_call","call_id":"call_owned","name":"owned_tool","arguments":"{\"location\":\"owned\"}","status":"completed"}`

func ownedOpenAIToolJSON(items string) string {
	return strings.Replace(completeResponsesJSON, `"output":[]`, `"output":[`+items+`]`, 1)
}

func ownedOpenAIToolStream() string {
	body := strings.Split(completeResponsesSSE, "event: response.completed")[0]
	body += ownedGatewayEvent("response.output_item.added", `"sequence_number":1,"output_index":0,"item":{"id":"item_owned","type":"function_call","call_id":"call_owned","name":"owned_tool","arguments":"","status":"in_progress"}`)
	body += ownedGatewayEvent("response.function_call_arguments.delta", `"sequence_number":2,"output_index":0,"item_id":"item_owned","delta":"{\"location\":"`)
	body += ownedGatewayEvent("response.function_call_arguments.delta", `"sequence_number":3,"output_index":0,"item_id":"item_owned","delta":"\"owned\"}"`)
	body += ownedGatewayEvent("response.function_call_arguments.done", `"sequence_number":4,"output_index":0,"item_id":"item_owned","arguments":"{\"location\":\"owned\"}"`)
	body += ownedGatewayEvent("response.output_item.done", `"sequence_number":5,"output_index":0,"item":`+ownedOpenAIToolItem)
	return body + ownedGatewayEvent("response.completed", `"sequence_number":6,"response":`+ownedOpenAIToolJSON(ownedOpenAIToolItem))
}

func ownedOpenAIInterleavedToolStream() string {
	items := []string{ownedOpenAIToolItem, strings.ReplaceAll(strings.ReplaceAll(ownedOpenAIToolItem, "item_owned", "item_other"), "call_owned", "call_other")}
	body := strings.Split(completeResponsesSSE, "event: response.completed")[0]
	sequence := 1
	appendEvent := func(eventType, fields string) {
		body += ownedGatewayEvent(eventType, fmt.Sprintf(`"sequence_number":%d,%s`, sequence, fields))
		sequence++
	}
	for index, item := range items {
		pending := strings.Replace(strings.Replace(item, `{\"location\":\"owned\"}`, "", 1), `"status":"completed"`, `"status":"in_progress"`, 1)
		appendEvent("response.output_item.added", fmt.Sprintf(`"output_index":%d,"item":%s`, index, pending))
	}
	for _, index := range []int{1, 0} {
		identity := "item_owned"
		if index == 1 {
			identity = "item_other"
		}
		fields := fmt.Sprintf(`"output_index":%d,"item_id":%q,`, index, identity)
		appendEvent("response.function_call_arguments.delta", fields+`"delta":"{\"location\":\"owned\"}"`)
		appendEvent("response.function_call_arguments.done", fields+`"arguments":"{\"location\":\"owned\"}"`)
		appendEvent("response.output_item.done", fmt.Sprintf(`"output_index":%d,"item":%s`, index, items[index]))
	}
	appendEvent("response.completed", `"response":`+ownedOpenAIToolJSON(strings.Join(items, ",")))
	return body
}

func TestCredentialGatewayOpenAIToolCompletion(test *testing.T) {
	other := strings.ReplaceAll(strings.ReplaceAll(ownedOpenAIToolItem, "item_owned", "item_other"), "call_owned", "call_other")
	for _, candidate := range []struct{ name, media, body string }{
		{"json", "application/json", ownedOpenAIToolJSON(ownedOpenAIToolItem)},
		{"stream", "text/event-stream", ownedOpenAIToolStream()},
		{"optional-status", "application/json", ownedOpenAIToolJSON(strings.Replace(ownedOpenAIToolItem, `,"status":"completed"`, "", 1))},
		{"optional-stream-status", "text/event-stream", strings.ReplaceAll(strings.Replace(ownedOpenAIToolStream(), `"arguments":"","status":"in_progress"`, `"arguments":""`, 1), ownedOpenAIToolItem, strings.Replace(ownedOpenAIToolItem, `,"status":"completed"`, "", 1))},
		{"multiple-tools", "application/json", ownedOpenAIToolJSON(ownedOpenAIToolItem + "," + other)},
		{"interleaved-tools", "text/event-stream", ownedOpenAIInterleavedToolStream()},
		{"empty-object", "application/json", ownedOpenAIToolJSON(strings.Replace(ownedOpenAIToolItem, `{\"location\":\"owned\"}`, `{}`, 1))},
		{"direct-caller", "application/json", ownedOpenAIToolJSON(strings.Replace(ownedOpenAIToolItem, `"type":"function_call"`, `"type":"function_call","async":false,"caller":{"type":"direct"}`, 1))},
	} {
		for _, chunkSize := range []int{1, 13, len(candidate.body)} {
			test.Run(fmt.Sprintf("%s-%d", candidate.name, chunkSize), func(test *testing.T) {
				request, _ := http.NewRequest(http.MethodPost, "http://owned/v1/responses", nil)
				protocol, err := newGatewayProtocol("codex", request, &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{candidate.media}}})
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
					test.Error("function stream wire bytes changed")
				}
				want := map[string]int64{"input_tokens": 7, "cache_creation_input_tokens": 2, "cache_read_input_tokens": 3, "output_tokens": 5}
				if !reflect.DeepEqual(protocol.usage, want) {
					test.Errorf("usage = %v, want %v", protocol.usage, want)
				}
				if protocol.toolBytes != 0 || len(protocol.toolInputs) != 0 {
					test.Error("closed function inputs retained buffered arguments")
				}
			})
		}
	}
}

func TestCredentialGatewayOpenAIToolRefusesMalformedOutcomes(test *testing.T) {
	stream := ownedOpenAIToolStream()
	missingDone := strings.Replace(stream, ownedGatewayEvent("response.function_call_arguments.done", `"sequence_number":4,"output_index":0,"item_id":"item_owned","arguments":"{\"location\":\"owned\"}"`), "", 1)
	missingDone = strings.Replace(strings.Replace(missingDone, `"sequence_number":5`, `"sequence_number":4`, 1), `"sequence_number":6`, `"sequence_number":5`, 1)
	deepArguments, _ := json.Marshal(strings.Repeat(`{"nested":`, 33) + `{}` + strings.Repeat(`}`, 33))
	for _, candidate := range []struct{ name, media, body string }{
		{"missing-id", "application/json", ownedOpenAIToolJSON(strings.Replace(ownedOpenAIToolItem, `"id":"item_owned",`, "", 1))},
		{"missing-call-id", "application/json", ownedOpenAIToolJSON(strings.Replace(ownedOpenAIToolItem, `"call_id":"call_owned",`, "", 1))},
		{"missing-name", "application/json", ownedOpenAIToolJSON(strings.Replace(ownedOpenAIToolItem, `"name":"owned_tool",`, "", 1))},
		{"array-arguments", "application/json", ownedOpenAIToolJSON(strings.Replace(ownedOpenAIToolItem, `{\"location\":\"owned\"}`, `[]`, 1))},
		{"null-arguments", "application/json", ownedOpenAIToolJSON(strings.Replace(ownedOpenAIToolItem, `{\"location\":\"owned\"}`, `null`, 1))},
		{"deep-arguments", "application/json", ownedOpenAIToolJSON(strings.Replace(ownedOpenAIToolItem, `"{\"location\":\"owned\"}"`, string(deepArguments), 1))},
		{"duplicate-argument-field", "application/json", ownedOpenAIToolJSON(strings.Replace(ownedOpenAIToolItem, `\"location\":\"owned\"`, `\"location\":\"owned\",\"LOCATION\":\"other\"`, 1))},
		{"duplicate-item-id", "application/json", ownedOpenAIToolJSON(ownedOpenAIToolItem + "," + strings.Replace(ownedOpenAIToolItem, "call_owned", "call_other", 1))},
		{"duplicate-call-id", "application/json", ownedOpenAIToolJSON(ownedOpenAIToolItem + "," + strings.Replace(ownedOpenAIToolItem, "item_owned", "item_other", 1))},
		{"unfinished-status", "application/json", ownedOpenAIToolJSON(strings.Replace(ownedOpenAIToolItem, `"status":"completed"`, `"status":"in_progress"`, 1))},
		{"unsupported-custom-tool", "application/json", ownedOpenAIToolJSON(strings.Replace(ownedOpenAIToolItem, `"type":"function_call"`, `"type":"custom_tool_call"`, 1))},
		{"unsupported-async", "application/json", ownedOpenAIToolJSON(strings.Replace(ownedOpenAIToolItem, `"type":"function_call"`, `"type":"function_call","async":true`, 1))},
		{"unsupported-program-caller", "application/json", ownedOpenAIToolJSON(strings.Replace(ownedOpenAIToolItem, `"type":"function_call"`, `"type":"function_call","caller":{"type":"program","caller_id":"program_other"}`, 1))},
		{"unsupported-namespace", "application/json", ownedOpenAIToolJSON(strings.Replace(ownedOpenAIToolItem, `"type":"function_call"`, `"type":"function_call","namespace":"other"`, 1))},
		{"nonempty-start", "text/event-stream", strings.Replace(stream, `"arguments":""`, `"arguments":"{}"`, 1)},
		{"wrong-item-delta", "text/event-stream", strings.Replace(stream, `"item_id":"item_owned"`, `"item_id":"other"`, 1)},
		{"wrong-delta-kind", "text/event-stream", strings.Replace(stream, "response.function_call_arguments.delta", "response.output_text.delta", 2)},
		{"changed-done-arguments", "text/event-stream", strings.Replace(stream, `"arguments":"{\"location\":\"owned\"}"`, `"arguments":"{}"`, 1)},
		{"changed-item-arguments", "text/event-stream", strings.Replace(stream, ownedOpenAIToolItem, strings.Replace(ownedOpenAIToolItem, `{\"location\":\"owned\"}`, `{}`, 1), 1)},
		{"changed-item-name", "text/event-stream", strings.Replace(stream, ownedOpenAIToolItem, strings.Replace(ownedOpenAIToolItem, "owned_tool", "other_tool", 1), 1)},
		{"changed-final-call-id", "text/event-stream", strings.Replace(stream, `"response":`+ownedOpenAIToolJSON(ownedOpenAIToolItem), `"response":`+ownedOpenAIToolJSON(strings.Replace(ownedOpenAIToolItem, "call_owned", "other_call", 1)), 1)},
		{"changed-final-type", "text/event-stream", strings.Replace(stream, `"response":`+ownedOpenAIToolJSON(ownedOpenAIToolItem), `"response":`+ownedOpenAIToolJSON(`{"id":"item_owned","type":"message","role":"assistant","status":"completed","content":[]}`), 1)},
		{"missing-arguments-done", "text/event-stream", missingDone},
		{"duplicate-stream-call-id", "text/event-stream", strings.ReplaceAll(ownedOpenAIInterleavedToolStream(), "call_other", "call_owned")},
		{"changed-final-arguments", "text/event-stream", strings.Replace(stream, `"response":`+ownedOpenAIToolJSON(ownedOpenAIToolItem), `"response":`+ownedOpenAIToolJSON(strings.Replace(ownedOpenAIToolItem, `{\"location\":\"owned\"}`, `{}`, 1)), 1)},
		{"changed-final-name", "text/event-stream", strings.Replace(stream, `"response":`+ownedOpenAIToolJSON(ownedOpenAIToolItem), `"response":`+ownedOpenAIToolJSON(strings.Replace(ownedOpenAIToolItem, "owned_tool", "other_tool", 1)), 1)},
		{"unfinished-item", "text/event-stream", strings.Split(stream, "event: response.output_item.done")[0]},
		{"missing-usage", "application/json", strings.Replace(ownedOpenAIToolJSON(ownedOpenAIToolItem), `"input_tokens":12,`, "", 1)},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			request, _ := http.NewRequest(http.MethodPost, "http://owned/v1/responses", nil)
			protocol, err := newGatewayProtocol("codex", request, &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{candidate.media}}})
			if err != nil {
				test.Fatal(err)
			}
			if protocol.feed([]byte(candidate.body)) == nil && protocol.finish() == nil {
				test.Fatal("accepted malformed function outcome")
			}
		})
	}
}

func TestCredentialGatewayOpenAIToolAggregateBufferBound(test *testing.T) {
	request, _ := http.NewRequest(http.MethodPost, "http://owned/v1/responses", nil)
	protocol, err := newGatewayProtocol("codex", request, &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}})
	if err != nil {
		test.Fatal(err)
	}
	start := strings.Split(ownedOpenAIToolStream(), "event: response.function_call_arguments.delta")[0]
	start += ownedGatewayEvent("response.output_item.added", `"sequence_number":2,"output_index":1,"item":{"id":"item_other","type":"function_call","call_id":"call_other","name":"owned_tool","arguments":"","status":"in_progress"}`)
	if err := protocol.feed([]byte(start)); err != nil {
		test.Fatal(err)
	}
	fragment, _ := json.Marshal(strings.Repeat(" ", gatewayFrameLimit/2+1))
	for sequence := 3; sequence <= 4; sequence++ {
		identity := "item_owned"
		if sequence == 4 {
			identity = "item_other"
		}
		frame := ownedGatewayEvent("response.function_call_arguments.delta", fmt.Sprintf(`"sequence_number":%d,"output_index":%d,"item_id":%q,"delta":%s`, sequence, sequence-3, identity, fragment))
		err := protocol.feed([]byte(frame))
		if sequence == 3 && err != nil || sequence == 4 && err == nil {
			test.Fatalf("aggregate bound: sequence %d, error %v", sequence, err)
		}
		protocol.ready = nil
	}
}
