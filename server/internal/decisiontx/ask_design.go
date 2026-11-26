package decisiontx

import (
	"bytes"
	"encoding/json"
)

// decodeDesign decodes options[].design (CHE-7064 D1.2). The nested object is as
// strict as the ask: unknown fields and wrong types are refused. frame_updated_at
// is stamped by the server after the design guard passes, so a caller value that
// is not zero is refused with DesignVersionServerOwnedCode.
func decodeDesign(prefix string, raw json.RawMessage, into **OptionDesign) error {
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil
	}
	field := prefix + ".design"
	fields, err := decodeSub(field, raw, "frame_url", "attestation_id", "frame_updated_at")
	if err != nil {
		return err
	}
	design := &OptionDesign{}
	for _, f := range []struct {
		name string
		into *string
	}{{"frame_url", &design.FrameURL}, {"attestation_id", &design.AttestationID}} {
		if err := decodeString(field+"."+f.name, fields[f.name], f.into); err != nil {
			return err
		}
	}
	if version := bytes.TrimSpace(fields["frame_updated_at"]); len(version) > 0 && string(version) != "null" {
		var number float64
		if err := json.Unmarshal(version, &number); err != nil {
			return &AskError{field + ".frame_updated_at", "must be a number"}
		}
		if number != 0 {
			return &CodedAskError{AskError{field + ".frame_updated_at", "is set by the server; remove it from the ask"}, DesignVersionServerOwnedCode}
		}
	}
	*into = design
	return nil
}
