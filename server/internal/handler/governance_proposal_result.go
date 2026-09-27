package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

const governanceProposalMaxReferences = 16

type governanceProposalResult struct {
	Label              string   `json:"label"`
	ActionID           string   `json:"action_id,omitempty"`
	TargetID           string   `json:"target_id,omitempty"`
	CitationIDs        []string `json:"citation_ids,omitempty"`
	ProposalConfidence float64  `json:"proposal_confidence"`
}

func decodeGovernanceProposalResult(reader io.Reader, candidateMapJSON, citationMapJSON []byte) (governanceProposalResult, error) {
	var result governanceProposalResult
	raw, err := decodeJSONObjectFromReader(reader, map[string]struct{}{
		"label": {}, "action_id": {}, "target_id": {}, "citation_ids": {}, "proposal_confidence": {},
	})
	if err != nil {
		return result, errors.New("invalid proposal result")
	}
	label, ok := raw["label"]
	if !ok || decodeJSONString(label, &result.Label) != nil {
		return result, errors.New("invalid proposal result")
	}
	if confidenceRaw, ok := raw["proposal_confidence"]; !ok || decodeProbability(confidenceRaw, &result.ProposalConfidence) != nil {
		return result, errors.New("invalid proposal result")
	}
	if actionRaw, ok := raw["action_id"]; ok {
		if err := decodeJSONString(actionRaw, &result.ActionID); err != nil {
			return result, errors.New("invalid proposal result")
		}
	}
	if targetRaw, ok := raw["target_id"]; ok {
		if err := decodeJSONString(targetRaw, &result.TargetID); err != nil {
			return result, errors.New("invalid proposal result")
		}
	}
	if citationsRaw, ok := raw["citation_ids"]; ok {
		if err := decodeJSONStringArray(citationsRaw, &result.CitationIDs); err != nil {
			return result, errors.New("invalid proposal result")
		}
	}
	if !validProposalLabel(result) {
		return result, errors.New("invalid proposal result")
	}
	candidates, err := decodeProposalCandidates(candidateMapJSON)
	if err != nil {
		return result, errors.New("proposal evidence unavailable")
	}
	citations, err := decodeProposalCitations(citationMapJSON)
	if err != nil {
		return result, errors.New("proposal evidence unavailable")
	}
	if result.Label == "correction" {
		if _, ok := candidates[result.ActionID+"\x00"+result.TargetID]; !ok {
			return result, errors.New("proposal reference was not offered")
		}
	}
	seenCitations := make(map[string]struct{}, len(result.CitationIDs))
	for _, citationID := range result.CitationIDs {
		if _, ok := citations[citationID]; !ok {
			return result, errors.New("proposal reference was not offered")
		}
		if _, duplicate := seenCitations[citationID]; duplicate {
			return result, errors.New("duplicate proposal citation")
		}
		seenCitations[citationID] = struct{}{}
	}
	return result, nil
}

func validProposalLabel(result governanceProposalResult) bool {
	switch result.Label {
	case "correction":
		return result.ActionID != "" && result.TargetID != "" && len(result.CitationIDs) > 0
	case "no_correction", "stale", "insufficient_evidence":
		return result.ActionID == "" && result.TargetID == ""
	default:
		return false
	}
}

func decodeProposalCandidates(encoded []byte) (map[string]struct{}, error) {
	items, err := decodeJSONArray(encoded)
	if err != nil || len(items) > governanceProposalMaxReferences {
		return nil, errors.New("invalid candidate map")
	}
	result := make(map[string]struct{}, len(items))
	for _, item := range items {
		fields, err := decodeJSONObject(item, map[string]struct{}{"action_id": {}, "target_id": {}})
		if err != nil || len(fields) != 2 {
			return nil, errors.New("invalid candidate map")
		}
		var actionID, targetID string
		if decodeJSONString(fields["action_id"], &actionID) != nil || decodeJSONString(fields["target_id"], &targetID) != nil ||
			actionID == "" || targetID == "" {
			return nil, errors.New("invalid candidate map")
		}
		result[actionID+"\x00"+targetID] = struct{}{}
	}
	return result, nil
}

func decodeProposalCitations(encoded []byte) (map[string]struct{}, error) {
	items, err := decodeJSONArray(encoded)
	if err != nil || len(items) > governanceProposalMaxReferences {
		return nil, errors.New("invalid citation map")
	}
	result := make(map[string]struct{}, len(items))
	for _, item := range items {
		fields, err := decodeJSONObject(item, map[string]struct{}{"citation_id": {}})
		if err != nil || len(fields) != 1 {
			return nil, errors.New("invalid citation map")
		}
		var citationID string
		if decodeJSONString(fields["citation_id"], &citationID) != nil || citationID == "" {
			return nil, errors.New("invalid citation map")
		}
		result[citationID] = struct{}{}
	}
	return result, nil
}

func decodeJSONArray(encoded []byte) ([]json.RawMessage, error) {
	if !bytes.HasPrefix(bytes.TrimSpace(encoded), []byte("[")) {
		return nil, errors.New("invalid proposal map")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(encoded, &items); err != nil || items == nil {
		return nil, errors.New("invalid proposal map")
	}
	return items, nil
}

func decodeJSONObject(encoded []byte, allowed map[string]struct{}) (map[string]json.RawMessage, error) {
	return decodeJSONObjectFromReader(bytes.NewReader(encoded), allowed)
}

func decodeJSONObjectFromReader(reader io.Reader, allowed map[string]struct{}) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("invalid proposal object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		keyValue, err := decoder.Token()
		key, ok := keyValue.(string)
		if err != nil || !ok {
			return nil, errors.New("invalid proposal object")
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, errors.New("duplicate proposal field")
		}
		if _, ok := allowed[key]; !ok {
			return nil, errors.New("invalid proposal object")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, errors.New("invalid proposal object")
		}
		fields[key] = value
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || requireJSONEOF(decoder) != nil {
		return nil, errors.New("invalid proposal object")
	}
	return fields, nil
}

func decodeJSONString(encoded json.RawMessage, target *string) error {
	if !bytes.HasPrefix(bytes.TrimSpace(encoded), []byte(`"`)) {
		return errors.New("expected string")
	}
	if err := json.Unmarshal(encoded, target); err != nil || *target != strings.TrimSpace(*target) {
		return errors.New("invalid string")
	}
	return nil
}

func decodeJSONStringArray(encoded json.RawMessage, target *[]string) error {
	if !bytes.HasPrefix(bytes.TrimSpace(encoded), []byte("[")) {
		return errors.New("expected array")
	}
	var values []json.RawMessage
	if err := json.Unmarshal(encoded, &values); err != nil || len(values) > governanceProposalMaxReferences {
		return errors.New("invalid array")
	}
	for _, value := range values {
		var decoded string
		if err := decodeJSONString(value, &decoded); err != nil || decoded == "" {
			return errors.New("invalid array item")
		}
		*target = append(*target, decoded)
	}
	return nil
}

func decodeProbability(encoded json.RawMessage, target *float64) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil || requireJSONEOF(decoder) != nil {
		return errors.New("invalid probability")
	}
	number, ok := value.(json.Number)
	if !ok {
		return errors.New("invalid probability")
	}
	parsed, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 || parsed > 1 {
		return errors.New("invalid probability")
	}
	*target = parsed
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}
