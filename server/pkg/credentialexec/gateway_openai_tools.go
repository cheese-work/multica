package credentialexec

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
)

type gatewayFunctionCall struct {
	callID    string
	name      string
	arguments [sha256.Size]byte
}

func parseGatewayFunction(item map[string]json.RawMessage, pending bool) (gatewayFunctionCall, error) {
	if len(item["namespace"]) != 0 || len(item["async"]) != 0 && !bytes.Equal(bytes.TrimSpace(item["async"]), []byte("false")) {
		return gatewayFunctionCall{}, ErrOutcomeUnknown
	}
	if !gatewayNull(item["caller"]) {
		caller, err := gatewayObject(item["caller"])
		if err != nil || len(caller) != 1 || gatewayString(caller, "type") != "direct" {
			return gatewayFunctionCall{}, ErrOutcomeUnknown
		}
	}
	function := gatewayFunctionCall{callID: gatewayString(item, "call_id"), name: gatewayString(item, "name")}
	status := "completed"
	if pending {
		status = "in_progress"
	}
	var arguments string
	if function.callID == "" || function.name == "" || len(item["status"]) != 0 && gatewayString(item, "status") != status || !gatewayText(item["arguments"]) || json.Unmarshal(item["arguments"], &arguments) != nil || len(arguments) > gatewayFrameLimit {
		return gatewayFunctionCall{}, ErrOutcomeUnknown
	}
	if pending {
		if arguments != "" {
			return gatewayFunctionCall{}, ErrOutcomeUnknown
		}
	} else if _, err := gatewayObject([]byte(arguments)); err != nil {
		return gatewayFunctionCall{}, ErrOutcomeUnknown
	}
	function.arguments = sha256.Sum256([]byte(arguments))
	return function, nil
}

func (protocol *gatewayProtocol) consumeFunctionItem(eventType string, index int64, item map[string]json.RawMessage) error {
	pending := eventType == "response.output_item.added"
	function, err := parseGatewayFunction(item, pending)
	identity := gatewayString(item, "id")
	if err != nil || identity == "" {
		return ErrOutcomeUnknown
	}
	if !pending {
		if protocol.blocks[index] != identity || protocol.phases[index] != 2 || protocol.functions[index] != function {
			return ErrOutcomeUnknown
		}
		delete(protocol.blocks, index)
		delete(protocol.phases, index)
		return nil
	}
	for _, previous := range protocol.outputIDs {
		if previous == identity {
			return ErrOutcomeUnknown
		}
	}
	if index != protocol.nextBlock || protocol.blocks[index] != "" || protocol.toolIDs[function.callID] {
		return ErrOutcomeUnknown
	}
	if protocol.functions == nil {
		protocol.functions = make(map[int64]gatewayFunctionCall)
		protocol.toolInputs = make(map[int64][]byte)
		protocol.toolIDs = make(map[string]bool)
	}
	protocol.nextBlock++
	protocol.functions[index] = function
	protocol.toolIDs[function.callID] = true
	protocol.toolInputs[index] = nil
	protocol.blocks[index], protocol.phases[index] = identity, 1
	protocol.outputIDs = append(protocol.outputIDs, identity)
	protocol.textHashes = append(protocol.textHashes, sha256.New())
	protocol.hasText = append(protocol.hasText, false)
	return nil
}

func (protocol *gatewayProtocol) consumeFunctionArguments(eventType string, object map[string]json.RawMessage) error {
	index, err := gatewayCount(object, "output_index")
	function, exists := protocol.functions[index]
	if err != nil || !exists || protocol.phases[index] != 1 || protocol.blocks[index] == "" || gatewayString(object, "item_id") != protocol.blocks[index] {
		return ErrOutcomeUnknown
	}
	field := "delta"
	if eventType == "response.function_call_arguments.done" {
		field = "arguments"
	}
	var arguments string
	if !gatewayText(object[field]) || json.Unmarshal(object[field], &arguments) != nil || len(arguments) > gatewayFrameLimit {
		return ErrOutcomeUnknown
	}
	if field == "delta" {
		if len(arguments) > gatewayFrameLimit-protocol.toolBytes {
			return ErrOutcomeUnknown
		}
		protocol.toolBytes += len(arguments)
		protocol.toolInputs[index] = append(protocol.toolInputs[index], arguments...)
		return nil
	}
	if arguments != string(protocol.toolInputs[index]) || len(object["name"]) != 0 && gatewayString(object, "name") != function.name {
		return ErrOutcomeUnknown
	}
	if _, err := gatewayObject([]byte(arguments)); err != nil {
		return ErrOutcomeUnknown
	}
	function.arguments = sha256.Sum256([]byte(arguments))
	protocol.functions[index] = function
	protocol.toolBytes -= len(protocol.toolInputs[index])
	delete(protocol.toolInputs, index)
	protocol.phases[index] = 2
	return nil
}
