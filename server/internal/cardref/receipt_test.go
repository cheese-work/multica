package cardref

import (
	"encoding/json"
	"testing"
)

const key = "shared-secret"

// Vectors computed with doop's own `macOf` (server/framePins.ts at
// 94530add75fe, HMAC-SHA256 over JSON.stringify of the field array) in Node.
var vectors = []struct {
	name string
	r    Receipt
	mac  string
}{
	{"R1", Receipt{Kind: "R1", CardRequestID: "card-1", PinID: "pin-1", State: "HELD", Epochs: map[string]int64{"f-b": 2, "f-a": 1}},
		"f448a97b7f7cabb25da3e9d9566629a161f3ab5681360edaae170ff0bb51b5e4"},
	{"R2", Receipt{Kind: "R2", CardRequestID: "card-1", PinID: "pin-1", State: "SEALED", Epochs: map[string]int64{"f-b": 2, "f-a": 1}, PickRequestID: "pick-1", PickedFrameID: "f-a"},
		"e5fa57d75599fbaec13b5592991bf2cbb0e943284c45e69fc77655498bb5c260"},
	// characters JSON encoders treat differently: a quote, <, >, &, U+2028, a
	// non-ASCII letter, a control character, a backslash; keys sorted by code unit.
	{"escapes", Receipt{Kind: "R1", CardRequestID: "c\"<>& é\u0001", PinID: "p", State: "HELD", Epochs: map[string]int64{"z": 3, "é": 4, `a\b`: 5}},
		"ffdf8e90906b9edf2e2becaced2b8b5a1aa0a1909a57f8b991e491fdd0c61146"},
}

func TestMACMatchesDoop(t *testing.T) {
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			if got := v.r.ComputeMAC(key); got != v.mac {
				t.Fatalf("mac = %s, want %s", got, v.mac)
			}
			signed := v.r
			signed.MAC = v.mac
			if !Verify(key, signed) {
				t.Fatal("doop's own mac does not verify")
			}
		})
	}
}

func TestVerifyRejects(t *testing.T) {
	good := vectors[1].r
	good.MAC = vectors[1].mac
	mutate := map[string]func(*Receipt){
		"kind":      func(r *Receipt) { r.Kind = "R1" },
		"card":      func(r *Receipt) { r.CardRequestID = "card-2" },
		"pin":       func(r *Receipt) { r.PinID = "pin-2" },
		"state":     func(r *Receipt) { r.State = "HELD" },
		"epoch":     func(r *Receipt) { r.Epochs = map[string]int64{"f-b": 2, "f-a": 9} },
		"extra":     func(r *Receipt) { r.Epochs = map[string]int64{"f-b": 2, "f-a": 1, "f-c": 1} },
		"pick":      func(r *Receipt) { r.PickRequestID = "pick-2" },
		"picked":    func(r *Receipt) { r.PickedFrameID = "f-b" },
		"empty mac": func(r *Receipt) { r.MAC = "" },
		"not hex":   func(r *Receipt) { r.MAC = "zz" },
		"short mac": func(r *Receipt) { r.MAC = r.MAC[:10] },
	}
	for name, f := range mutate {
		r := good
		f(&r)
		if Verify(key, r) {
			t.Errorf("%s: forged receipt verified", name)
		}
	}
	if Verify("other-secret", good) || Verify("", good) {
		t.Error("wrong or empty key verified")
	}
}

func TestReceiptJSONIsDoopShape(t *testing.T) {
	raw := `{"kind":"R2","cardRequestId":"card-1","pinId":"pin-1","state":"SEALED","epochs":{"f-b":2,"f-a":1},"pickRequestId":"pick-1","pickedFrameId":"f-a","mac":"` + vectors[1].mac + `"}`
	var r Receipt
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	if !Verify(key, r) {
		t.Fatalf("receipt read from doop's JSON does not verify: %+v", r)
	}
}
