// Package decisionaction holds the allowlist registry and the canonical
// payload hash for decision actions: privileged changes an agent prepares and
// the request's human approves on a decision card.
package decisionaction

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// maxSafeInteger is 2^53-1: the largest integer every JCS consumer reads back
// exactly. Payloads carry no fractions, so integers are the only numbers.
const maxSafeInteger = 1<<53 - 1

// Canonical returns the RFC 8785 (JCS) form of one JSON document. It refuses
// what a payload never needs and JCS number formatting would make ambiguous:
// fractions, exponents, integers past 2^53-1, duplicate keys, trailing data.
func Canonical(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var out bytes.Buffer
	if err := writeValue(&out, decoder); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("canonical json: trailing data")
	}
	return out.Bytes(), nil
}

func writeValue(out *bytes.Buffer, decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("canonical json: %w", err)
	}
	switch value := token.(type) {
	case json.Delim:
		if value == '{' {
			return writeObject(out, decoder)
		}
		return writeArray(out, decoder)
	case string:
		writeString(out, value)
	case json.Number:
		text, err := canonicalInteger(value.String())
		if err != nil {
			return err
		}
		out.WriteString(text)
	case bool:
		out.WriteString(strconv.FormatBool(value))
	case nil:
		out.WriteString("null")
	}
	return nil
}

func writeArray(out *bytes.Buffer, decoder *json.Decoder) error {
	out.WriteByte('[')
	for first := true; decoder.More(); first = false {
		if !first {
			out.WriteByte(',')
		}
		if err := writeValue(out, decoder); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("canonical json: %w", err)
	}
	out.WriteByte(']')
	return nil
}

func writeObject(out *bytes.Buffer, decoder *json.Decoder) error {
	members := map[string][]byte{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("canonical json: %w", err)
		}
		key, isKey := token.(string)
		if !isKey {
			return fmt.Errorf("canonical json: object key is %T, not a string", token)
		}
		if _, dup := members[key]; dup {
			return fmt.Errorf("canonical json: duplicate key %q", key)
		}
		var member bytes.Buffer
		if err := writeValue(&member, decoder); err != nil {
			return err
		}
		members[key] = member.Bytes()
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("canonical json: %w", err)
	}
	keys := make([]string, 0, len(members))
	for key := range members {
		keys = append(keys, key)
	}
	// JCS sorts member names by UTF-16 code unit, not by UTF-8 byte.
	sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
	out.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			out.WriteByte(',')
		}
		writeString(out, key)
		out.WriteByte(':')
		out.Write(members[key])
	}
	out.WriteByte('}')
	return nil
}

func utf16Less(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

// writeString emits the JCS string form: only quote, backslash and control
// characters are escaped, never <, > or &.
func writeString(out *bytes.Buffer, text string) {
	out.WriteByte('"')
	for _, r := range text {
		switch {
		case r == '"':
			out.WriteString(`\"`)
		case r == '\\':
			out.WriteString(`\\`)
		case r == '\b':
			out.WriteString(`\b`)
		case r == '\t':
			out.WriteString(`\t`)
		case r == '\n':
			out.WriteString(`\n`)
		case r == '\f':
			out.WriteString(`\f`)
		case r == '\r':
			out.WriteString(`\r`)
		case r < 0x20:
			fmt.Fprintf(out, `\u%04x`, r)
		default:
			out.WriteRune(r)
		}
	}
	out.WriteByte('"')
}

func canonicalInteger(text string) (string, error) {
	if strings.ContainsAny(text, ".eE") {
		return "", fmt.Errorf("canonical json: %s is not an integer", text)
	}
	number, err := strconv.ParseInt(text, 10, 64)
	if err != nil || number > maxSafeInteger || number < -maxSafeInteger {
		return "", fmt.Errorf("canonical json: %s is outside the safe integer range", text)
	}
	return strconv.FormatInt(number, 10), nil
}
