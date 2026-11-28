package cardref

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Receipt is the proof doop gives that a pin is held (R1, state HELD) or sealed
// to a pick (R2, state SEALED). The JSON names are doop's `Receipt` type in
// server/framePins.ts.
type Receipt struct {
	Kind          string           `json:"kind"`
	CardRequestID string           `json:"cardRequestId"`
	PinID         string           `json:"pinId"`
	State         string           `json:"state"`
	Epochs        map[string]int64 `json:"epochs"`
	PickRequestID string           `json:"pickRequestId,omitempty"`
	PickedFrameID string           `json:"pickedFrameId,omitempty"`
	MAC           string           `json:"mac"`
}

// ComputeMAC is doop's `macOf`: HMAC-SHA256, hex, over the bytes JS
// JSON.stringify gives for
//
//	[kind, cardRequestId, pinId, state, [[frameId, epoch], ...], pickRequestId|null, pickedFrameId|null]
//
// with the epoch pairs sorted by frame id (UTF-16 code units, as JS `<` sorts).
// It must match that byte for byte, so the JSON is written here and not left to
// encoding/json (which escapes <, >, & and U+2028).
func (r Receipt) ComputeMAC(key string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, s := range []string{r.Kind, r.CardRequestID, r.PinID, r.State} {
		if i > 0 {
			b.WriteByte(',')
		}
		jsString(&b, s)
	}
	b.WriteString(",[")
	frames := make([]string, 0, len(r.Epochs))
	for f := range r.Epochs {
		frames = append(frames, f)
	}
	sort.Slice(frames, func(i, j int) bool { return lessUTF16(frames[i], frames[j]) })
	for i, f := range frames {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('[')
		jsString(&b, f)
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(r.Epochs[f], 10))
		b.WriteByte(']')
	}
	b.WriteString("],")
	for i, s := range []string{r.PickRequestID, r.PickedFrameID} {
		if i > 0 {
			b.WriteByte(',')
		}
		if s == "" {
			b.WriteString("null")
		} else {
			jsString(&b, s)
		}
	}
	b.WriteByte(']')
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(b.String()))
	return hex.EncodeToString(m.Sum(nil))
}

// Verify is true only for a receipt this key signed, field for field.
func Verify(key string, r Receipt) bool {
	if key == "" {
		return false
	}
	got, err := hex.DecodeString(r.MAC)
	if err != nil {
		return false
	}
	want, _ := hex.DecodeString(r.ComputeMAC(key))
	return hmac.Equal(want, got)
}

// jsString writes s as JS JSON.stringify does: only the quote, the backslash and
// the control characters are escaped; everything else is written as it is.
func jsString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, c := range s {
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\f':
			b.WriteString(`\f`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte("0123456789abcdef"[c>>4])
			b.WriteByte("0123456789abcdef"[c&0xf])
		default:
			b.WriteRune(c)
		}
	}
	b.WriteByte('"')
}

func lessUTF16(a, b string) bool {
	x, y := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return len(x) < len(y)
}
