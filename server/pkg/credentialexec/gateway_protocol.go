package credentialexec

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"hash"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

const gatewayFrameLimit = 1 << 20

type gatewayProtocol struct {
	provider   string
	path       string
	stream     bool
	buffer     []byte
	data       []byte
	wire       []byte
	ready      []byte
	event      string
	started    bool
	complete   bool
	finalDelta bool
	responseID string
	model      string
	sequence   int64
	nextBlock  int64
	blocks     map[int64]string
	phases     map[int64]int
	outputIDs  []string
	textHashes []hash.Hash
	hasText    []bool
	usage      map[string]int64
	toolIDs    map[string]bool
	toolInputs map[int64][]byte
	toolBytes  int
	functions  map[int64]gatewayFunctionCall
}

func newGatewayProtocol(provider string, request *http.Request, response *http.Response) (*gatewayProtocol, error) {
	mediaType, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || parameters["charset"] != "" && !strings.EqualFold(parameters["charset"], "utf-8") || response.StatusCode != http.StatusOK || len(response.Trailer) != 0 {
		return nil, ErrOutcomeUnknown
	}
	protocol := &gatewayProtocol{provider: provider, path: request.URL.Path, blocks: make(map[int64]string), phases: make(map[int64]int), usage: make(map[string]int64)}
	if request.Method == http.MethodGet && protocol.path == "/v1/models" || provider == "claude" && protocol.path == "/v1/messages/count_tokens" {
		if mediaType != "application/json" {
			return nil, ErrOutcomeUnknown
		}
		return protocol, nil
	}
	if provider != "claude" && provider != "codex" || provider == "claude" && protocol.path != "/v1/messages" || provider == "codex" && protocol.path != "/v1/responses" {
		return nil, ErrOutcomeUnknown
	}
	if mediaType != "application/json" && mediaType != "text/event-stream" {
		return nil, ErrOutcomeUnknown
	}
	protocol.stream = mediaType == "text/event-stream"
	return protocol, nil
}

func (protocol *gatewayProtocol) feed(contents []byte) error {
	if !protocol.stream {
		if len(contents) > gatewayFrameLimit-len(protocol.buffer) {
			return ErrOutcomeUnknown
		}
		protocol.buffer = append(protocol.buffer, contents...)
		return nil
	}
	for _, character := range contents {
		if len(protocol.wire) >= gatewayFrameLimit {
			return ErrOutcomeUnknown
		}
		protocol.wire = append(protocol.wire, character)
		if character != '\n' {
			protocol.buffer = append(protocol.buffer, character)
			continue
		}
		line := strings.TrimSuffix(string(protocol.buffer), "\r")
		protocol.buffer = protocol.buffer[:0]
		if !utf8.ValidString(line) || strings.ContainsAny(line, "\x00\r") {
			return ErrOutcomeUnknown
		}
		if line == "" {
			if len(protocol.data) != 0 {
				if err := protocol.consumeEvent(); err != nil {
					return err
				}
			} else if protocol.event != "" {
				return ErrOutcomeUnknown
			}
			protocol.data, protocol.event = protocol.data[:0], ""
			protocol.ready = append(protocol.ready, protocol.wire...)
			protocol.wire = protocol.wire[:0]
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			if protocol.event != "" || len(value) > 256 {
				return ErrOutcomeUnknown
			}
			protocol.event = value
		case "data":
			protocol.data = append(protocol.data, []byte(value)...)
			protocol.data = append(protocol.data, '\n')
		default:
			return ErrOutcomeUnknown
		}
	}
	return nil
}

func (protocol *gatewayProtocol) finish() error {
	if protocol.stream {
		if len(protocol.wire) != 0 || !protocol.complete {
			return ErrOutcomeUnknown
		}
		return nil
	}
	object, err := gatewayObject(protocol.buffer)
	if err != nil || !gatewayNull(object["error"]) {
		return ErrOutcomeUnknown
	}
	if protocol.path == "/v1/models" {
		var entries []json.RawMessage
		if json.Unmarshal(object["data"], &entries) != nil || entries == nil {
			return ErrOutcomeUnknown
		}
		return nil
	}
	if protocol.path == "/v1/messages/count_tokens" {
		_, err := gatewayCount(object, "input_tokens")
		return err
	}
	if protocol.provider == "claude" {
		if gatewayString(object, "type") != "message" || gatewayString(object, "role") != "assistant" || protocol.setIdentity(object) != nil || protocol.validateClaudeContent(object["content"]) != nil || !protocol.validClaudeStopReason(gatewayString(object, "stop_reason")) {
			return ErrOutcomeUnknown
		}
		usage, err := gatewayObject(object["usage"])
		if err != nil || protocol.updateClaudeUsage(usage) != nil {
			return ErrOutcomeUnknown
		}
		return protocol.validateClaudeUsage()
	}
	return protocol.validateResponse(object)
}

func (protocol *gatewayProtocol) consumeEvent() error {
	if protocol.complete {
		return ErrOutcomeUnknown
	}
	object, err := gatewayObject(protocol.data)
	if err != nil || !gatewayNull(object["error"]) {
		return ErrOutcomeUnknown
	}
	eventType := gatewayString(object, "type")
	if protocol.event != "" && protocol.event != eventType {
		return ErrOutcomeUnknown
	}
	if protocol.provider == "claude" {
		return protocol.consumeClaude(eventType, object)
	}
	sequence, err := gatewayCount(object, "sequence_number")
	if err != nil || sequence != protocol.sequence || sequence == math.MaxInt64 {
		return ErrOutcomeUnknown
	}
	protocol.sequence++
	if eventType == "response.created" {
		response, err := gatewayObject(object["response"])
		if err != nil || protocol.started || gatewayString(response, "object") != "response" || gatewayString(response, "status") != "in_progress" || protocol.setIdentity(response) != nil {
			return ErrOutcomeUnknown
		}
		protocol.started = true
		return nil
	}
	if !protocol.started {
		return ErrOutcomeUnknown
	}
	switch eventType {
	case "response.in_progress":
		response, err := gatewayObject(object["response"])
		if err != nil || protocol.setIdentity(response) != nil || gatewayString(response, "status") != "in_progress" {
			return ErrOutcomeUnknown
		}
	case "response.output_item.added", "response.output_item.done":
		index, err := gatewayCount(object, "output_index")
		if err != nil || index >= 4096 {
			return ErrOutcomeUnknown
		}
		item, err := gatewayObject(object["item"])
		if err == nil && gatewayString(item, "type") == "function_call" {
			return protocol.consumeFunctionItem(eventType, index, item)
		}
		if err != nil || gatewayString(item, "type") != "message" || gatewayString(item, "role") != "assistant" || gatewayString(item, "id") == "" {
			return ErrOutcomeUnknown
		}
		if _, exists := protocol.functions[index]; exists {
			return ErrOutcomeUnknown
		}
		identity := gatewayString(item, "id")
		if eventType == "response.output_item.added" {
			for _, previous := range protocol.outputIDs {
				if previous == identity {
					return ErrOutcomeUnknown
				}
			}
			if index != protocol.nextBlock || protocol.blocks[index] != "" || gatewayString(item, "status") != "in_progress" || !gatewayEmptyArray(item["content"]) {
				return ErrOutcomeUnknown
			}
			protocol.nextBlock++
			protocol.blocks[index] = identity
			protocol.outputIDs = append(protocol.outputIDs, identity)
			protocol.textHashes = append(protocol.textHashes, sha256.New())
			protocol.hasText = append(protocol.hasText, false)
		} else {
			if protocol.blocks[index] != identity || gatewayString(item, "status") != "completed" || !protocol.matchesResponseContent(item["content"], int(index)) || protocol.phases[index] != 0 && protocol.phases[index] != 3 {
				return ErrOutcomeUnknown
			}
			delete(protocol.blocks, index)
			delete(protocol.phases, index)
		}
	case "response.output_text.delta", "response.output_text.done", "response.content_part.added", "response.content_part.done":
		index, err := gatewayCount(object, "output_index")
		contentIndex, contentErr := gatewayCount(object, "content_index")
		_, function := protocol.functions[index]
		if err != nil || contentErr != nil || contentIndex != 0 || function || protocol.blocks[index] == "" || gatewayString(object, "item_id") != protocol.blocks[index] {
			return ErrOutcomeUnknown
		}
		if strings.HasPrefix(eventType, "response.content_part.") {
			part, err := gatewayObject(object["part"])
			if err != nil || gatewayString(part, "type") != "output_text" || !gatewayText(part["text"]) {
				return ErrOutcomeUnknown
			}
			if eventType == "response.content_part.added" {
				var text string
				_ = json.Unmarshal(part["text"], &text)
				if protocol.phases[index] != 0 || text != "" {
					return ErrOutcomeUnknown
				}
				protocol.phases[index] = 1
				protocol.hasText[index] = true
			} else {
				if protocol.phases[index] != 2 || !protocol.matchesText(part["text"], int(index)) {
					return ErrOutcomeUnknown
				}
				protocol.phases[index] = 3
			}
		} else {
			if protocol.phases[index] != 1 {
				return ErrOutcomeUnknown
			}
			field := "delta"
			if eventType == "response.output_text.done" {
				field = "text"
				protocol.phases[index] = 2
			}
			if !gatewayText(object[field]) {
				return ErrOutcomeUnknown
			}
			if eventType == "response.output_text.delta" {
				var text string
				_ = json.Unmarshal(object[field], &text)
				_, _ = protocol.textHashes[index].Write([]byte(text))
			} else if !protocol.matchesText(object[field], int(index)) {
				return ErrOutcomeUnknown
			}
		}
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		return protocol.consumeFunctionArguments(eventType, object)
	case "response.completed":
		response, err := gatewayObject(object["response"])
		if err != nil || len(protocol.blocks) != 0 || protocol.validateResponse(response) != nil {
			return ErrOutcomeUnknown
		}
		protocol.complete = true
	default:
		return ErrOutcomeUnknown
	}
	return nil
}

func (protocol *gatewayProtocol) consumeClaude(eventType string, object map[string]json.RawMessage) error {
	if eventType == "ping" {
		return nil
	}
	if eventType == "message_start" {
		message, err := gatewayObject(object["message"])
		if err != nil || protocol.started || gatewayString(message, "type") != "message" || gatewayString(message, "role") != "assistant" || protocol.setIdentity(message) != nil || !gatewayEmptyArray(message["content"]) || !gatewayNull(message["stop_reason"]) {
			return ErrOutcomeUnknown
		}
		usage, err := gatewayObject(message["usage"])
		if err != nil || protocol.updateClaudeUsage(usage) != nil {
			return ErrOutcomeUnknown
		}
		protocol.started = true
		return nil
	}
	if !protocol.started {
		return ErrOutcomeUnknown
	}
	switch eventType {
	case "content_block_start", "content_block_delta", "content_block_stop":
		index, err := gatewayCount(object, "index")
		if err != nil || index >= 4096 || protocol.finalDelta {
			return ErrOutcomeUnknown
		}
		if eventType == "content_block_start" {
			block, err := gatewayObject(object["content_block"])
			if err != nil || index != protocol.nextBlock || protocol.blocks[index] != "" {
				return ErrOutcomeUnknown
			}
			blockType := gatewayString(block, "type")
			switch blockType {
			case "text":
				if !gatewayText(block["text"]) {
					return ErrOutcomeUnknown
				}
			case "tool_use":
				if protocol.registerClaudeTool(block, true) != nil {
					return ErrOutcomeUnknown
				}
				protocol.toolInputs[index] = nil
			default:
				return ErrOutcomeUnknown
			}
			protocol.nextBlock++
			protocol.blocks[index] = blockType
		} else if protocol.blocks[index] == "" {
			return ErrOutcomeUnknown
		} else if eventType == "content_block_stop" {
			if protocol.blocks[index] == "tool_use" {
				contents := protocol.toolInputs[index]
				if len(contents) == 0 {
					contents = []byte("{}")
				}
				if _, err := gatewayObject(contents); err != nil {
					return ErrOutcomeUnknown
				}
				protocol.toolBytes -= len(protocol.toolInputs[index])
				delete(protocol.toolInputs, index)
			}
			delete(protocol.blocks, index)
		} else {
			delta, err := gatewayObject(object["delta"])
			if err != nil {
				return ErrOutcomeUnknown
			}
			if protocol.blocks[index] == "text" {
				if gatewayString(delta, "type") != "text_delta" || !gatewayText(delta["text"]) {
					return ErrOutcomeUnknown
				}
			} else {
				if gatewayString(delta, "type") != "input_json_delta" || !gatewayText(delta["partial_json"]) {
					return ErrOutcomeUnknown
				}
				var fragment string
				_ = json.Unmarshal(delta["partial_json"], &fragment)
				if len(fragment) > gatewayFrameLimit-protocol.toolBytes {
					return ErrOutcomeUnknown
				}
				protocol.toolInputs[index] = append(protocol.toolInputs[index], fragment...)
				protocol.toolBytes += len(fragment)
			}
		}
	case "message_delta":
		delta, err := gatewayObject(object["delta"])
		if err != nil || protocol.finalDelta || len(protocol.blocks) != 0 || !protocol.validClaudeStopReason(gatewayString(delta, "stop_reason")) {
			return ErrOutcomeUnknown
		}
		usage, err := gatewayObject(object["usage"])
		if err != nil || protocol.updateClaudeUsage(usage) != nil {
			return ErrOutcomeUnknown
		}
		protocol.finalDelta = true
	case "message_stop":
		if !protocol.finalDelta || len(protocol.blocks) != 0 || protocol.validateClaudeUsage() != nil {
			return ErrOutcomeUnknown
		}
		protocol.complete = true
	default:
		return ErrOutcomeUnknown
	}
	return nil
}

func (protocol *gatewayProtocol) registerClaudeTool(block map[string]json.RawMessage, stream bool) error {
	input, err := gatewayObject(block["input"])
	identity := gatewayString(block, "id")
	if err != nil || stream && len(input) != 0 || identity == "" || gatewayString(block, "name") == "" || protocol.toolIDs[identity] {
		return ErrOutcomeUnknown
	}
	if protocol.toolIDs == nil {
		protocol.toolIDs = make(map[string]bool)
		protocol.toolInputs = make(map[int64][]byte)
	}
	protocol.toolIDs[identity] = true
	return nil
}

func (protocol *gatewayProtocol) validateClaudeContent(contents json.RawMessage) error {
	var blocks []json.RawMessage
	if json.Unmarshal(contents, &blocks) != nil || blocks == nil || len(blocks) > 4096 {
		return ErrOutcomeUnknown
	}
	for _, contents := range blocks {
		block, err := gatewayObject(contents)
		if err != nil {
			return ErrOutcomeUnknown
		}
		switch gatewayString(block, "type") {
		case "text":
			if !gatewayText(block["text"]) {
				return ErrOutcomeUnknown
			}
		case "tool_use":
			if protocol.registerClaudeTool(block, false) != nil {
				return ErrOutcomeUnknown
			}
		default:
			return ErrOutcomeUnknown
		}
	}
	return nil
}

func (protocol *gatewayProtocol) validClaudeStopReason(reason string) bool {
	if reason == "tool_use" {
		return len(protocol.toolIDs) != 0
	}
	return len(protocol.toolIDs) == 0 && claudeStopReason(reason)
}

func (protocol *gatewayProtocol) setIdentity(object map[string]json.RawMessage) error {
	identity, model := gatewayString(object, "id"), gatewayString(object, "model")
	if identity == "" || model == "" || protocol.responseID != "" && (protocol.responseID != identity || protocol.model != model) {
		return ErrOutcomeUnknown
	}
	protocol.responseID, protocol.model = identity, model
	return nil
}

func (protocol *gatewayProtocol) updateClaudeUsage(object map[string]json.RawMessage) error {
	if !gatewayNull(object["server_tool_use"]) {
		return ErrOutcomeUnknown
	}
	for _, field := range []string{"input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "output_tokens"} {
		if contents, exists := object[field]; exists {
			count, err := gatewayCount(map[string]json.RawMessage{field: contents}, field)
			if err != nil || count < protocol.usage[field] {
				return ErrOutcomeUnknown
			}
			protocol.usage[field] = count
		}
	}
	if _, err := gatewayCount(object, "output_tokens"); err != nil {
		return err
	}
	return nil
}

func (protocol *gatewayProtocol) validateClaudeUsage() error {
	if protocol.responseID == "" {
		return ErrOutcomeUnknown
	}
	total := int64(0)
	for _, field := range []string{"input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "output_tokens"} {
		count, exists := protocol.usage[field]
		if !exists || count > math.MaxInt64-total {
			return ErrOutcomeUnknown
		}
		total += count
	}
	return nil
}

func (protocol *gatewayProtocol) validateResponse(object map[string]json.RawMessage) error {
	if protocol.setIdentity(object) != nil || gatewayString(object, "object") != "response" || gatewayString(object, "status") != "completed" || !gatewayNull(object["error"]) || !gatewayNull(object["incomplete_details"]) {
		return ErrOutcomeUnknown
	}
	var output []json.RawMessage
	if json.Unmarshal(object["output"], &output) != nil || output == nil || len(output) > 4096 || protocol.started && len(output) != len(protocol.outputIDs) {
		return ErrOutcomeUnknown
	}
	identities := make(map[string]bool)
	callIDs := make(map[string]bool)
	for index, contents := range output {
		item, err := gatewayObject(contents)
		if err != nil || gatewayString(item, "id") == "" {
			return ErrOutcomeUnknown
		}
		identity := gatewayString(item, "id")
		if identities[identity] || protocol.started && identity != protocol.outputIDs[index] {
			return ErrOutcomeUnknown
		}
		identities[identity] = true
		expectedFunction, function := protocol.functions[int64(index)]
		if gatewayString(item, "type") == "function_call" {
			actual, err := parseGatewayFunction(item, false)
			if err != nil || callIDs[actual.callID] || protocol.started && (!function || actual != expectedFunction) {
				return ErrOutcomeUnknown
			}
			callIDs[actual.callID] = true
			continue
		}
		if function || gatewayString(item, "type") != "message" || gatewayString(item, "role") != "assistant" || gatewayString(item, "status") != "completed" || validateGatewayContent(item["content"], "output_text") != nil || protocol.started && !protocol.matchesResponseContent(item["content"], index) {
			return ErrOutcomeUnknown
		}
	}
	usage, err := gatewayObject(object["usage"])
	if err != nil {
		return err
	}
	input, inputErr := gatewayCount(usage, "input_tokens")
	outputTokens, outputErr := gatewayCount(usage, "output_tokens")
	total, totalErr := gatewayCount(usage, "total_tokens")
	details, detailsErr := gatewayObject(usage["input_tokens_details"])
	cacheRead, readErr := gatewayCount(details, "cached_tokens")
	cacheWrite, writeErr := gatewayCount(details, "cache_write_tokens")
	outputDetails, outputDetailsErr := gatewayObject(usage["output_tokens_details"])
	reasoning, reasoningErr := gatewayCount(outputDetails, "reasoning_tokens")
	if inputErr != nil || outputErr != nil || totalErr != nil || detailsErr != nil || readErr != nil || writeErr != nil || outputDetailsErr != nil || reasoningErr != nil || input > math.MaxInt64-outputTokens || total != input+outputTokens || cacheRead > input || cacheWrite > input-cacheRead || reasoning > outputTokens {
		return ErrOutcomeUnknown
	}
	protocol.usage = map[string]int64{"input_tokens": input - cacheRead - cacheWrite, "cache_creation_input_tokens": cacheWrite, "cache_read_input_tokens": cacheRead, "output_tokens": outputTokens}
	return nil
}

func (protocol *gatewayProtocol) matchesText(contents json.RawMessage, index int) bool {
	var text string
	if !gatewayText(contents) || json.Unmarshal(contents, &text) != nil {
		return false
	}
	digest := sha256.Sum256([]byte(text))
	return bytes.Equal(protocol.textHashes[index].Sum(nil), digest[:])
}

func (protocol *gatewayProtocol) matchesResponseContent(contents json.RawMessage, index int) bool {
	if !protocol.hasText[index] {
		return gatewayEmptyArray(contents)
	}
	var entries []map[string]json.RawMessage
	return json.Unmarshal(contents, &entries) == nil && len(entries) == 1 && gatewayString(entries[0], "type") == "output_text" && protocol.matchesText(entries[0]["text"], index)
}

func claudeStopReason(reason string) bool {
	switch reason {
	case "end_turn", "max_tokens", "stop_sequence", "refusal", "model_context_window_exceeded":
		return true
	}
	return false
}

func gatewayNull(contents json.RawMessage) bool {
	return len(contents) == 0 || bytes.Equal(bytes.TrimSpace(contents), []byte("null"))
}

func gatewayText(contents json.RawMessage) bool {
	var text string
	return !gatewayNull(contents) && json.Unmarshal(contents, &text) == nil && utf8.ValidString(text)
}

func gatewayEmptyArray(contents json.RawMessage) bool {
	var entries []json.RawMessage
	return json.Unmarshal(contents, &entries) == nil && entries != nil && len(entries) == 0
}

func validateGatewayContent(contents json.RawMessage, expected string) error {
	var blocks []json.RawMessage
	if json.Unmarshal(contents, &blocks) != nil || blocks == nil || len(blocks) > 4096 {
		return ErrOutcomeUnknown
	}
	for _, contents := range blocks {
		block, err := gatewayObject(contents)
		if err != nil || gatewayString(block, "type") != expected || !gatewayText(block["text"]) {
			return ErrOutcomeUnknown
		}
	}
	return nil
}

func gatewayString(object map[string]json.RawMessage, field string) string {
	var value string
	if json.Unmarshal(object[field], &value) != nil || value == "" || len(value) > 1024 || strings.ContainsAny(value, "\x00\r\n") {
		return ""
	}
	return value
}

func gatewayCount(object map[string]json.RawMessage, field string) (int64, error) {
	contents := object[field]
	if len(contents) == 0 || len(contents) > 19 {
		return 0, ErrOutcomeUnknown
	}
	for _, character := range contents {
		if character < '0' || character > '9' {
			return 0, ErrOutcomeUnknown
		}
	}
	count, err := strconv.ParseInt(string(contents), 10, 64)
	if err != nil {
		return 0, ErrOutcomeUnknown
	}
	return count, nil
}

func gatewayObject(contents []byte) (map[string]json.RawMessage, error) {
	if len(contents) == 0 || !utf8.Valid(contents) || len(contents) > gatewayFrameLimit {
		return nil, ErrOutcomeUnknown
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.UseNumber()
	if validateGatewayJSON(decoder, 0) != nil {
		return nil, ErrOutcomeUnknown
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, ErrOutcomeUnknown
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(contents, &object) != nil || object == nil {
		return nil, ErrOutcomeUnknown
	}
	return object, nil
}

func validateGatewayJSON(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return ErrOutcomeUnknown
	}
	token, err := decoder.Token()
	if err != nil {
		return ErrOutcomeUnknown
	}
	delimiter, nested := token.(json.Delim)
	if !nested {
		return nil
	}
	if delimiter != '{' && delimiter != '[' {
		return ErrOutcomeUnknown
	}
	fields := make(map[string]bool)
	for decoder.More() {
		if delimiter == '{' {
			key, err := decoder.Token()
			name, valid := key.(string)
			name = strings.ToLower(name)
			if err != nil || !valid || fields[name] {
				return ErrOutcomeUnknown
			}
			fields[name] = true
		}
		if validateGatewayJSON(decoder, depth+1) != nil {
			return ErrOutcomeUnknown
		}
	}
	ending, err := decoder.Token()
	if err != nil || delimiter == '{' && ending != json.Delim('}') || delimiter == '[' && ending != json.Delim(']') {
		return ErrOutcomeUnknown
	}
	return nil
}
