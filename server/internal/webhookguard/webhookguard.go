// Package webhookguard holds the pure checks of the inbound workflow webhook
// (see PR #425, "Workflow inbound webhook guard — security spec"). It touches
// no database and no network; the handler orders the checks.
package webhookguard

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	MaxBodyBytes = 16 << 10
	MaxSkew      = 300 * time.Second
	MaxRefLen    = 128
	// NonceRetention outlives the timestamp window (twice MaxSkew plus
	// margin), so a captured call cannot be replayed once its nonce is gone.
	NonceRetention = 900 * time.Second
)

var (
	ErrBadRequest   = errors.New("webhookguard: bad request")
	ErrUnauthorized = errors.New("webhookguard: unauthorized")
)

var (
	nonceRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)
	refRe   = regexp.MustCompile(`^[A-Za-z0-9._:/#-]{1,128}$`)
	issueRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,9}-[0-9]{1,9}$`)
)

type Result string

const (
	ResultSuccess Result = "success"
	ResultFailure Result = "failure"
)

type Payload struct {
	Issue  string
	Result Result
	Ref    string
}

func mac(secret []byte, timestamp, nonce string, body []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte("v1." + timestamp + "." + nonce + "."))
	h.Write(body)
	return h.Sum(nil)
}

// Sign returns the X-Multica-Signature header value.
func Sign(secret []byte, timestamp, nonce string, body []byte) string {
	return "v1=" + hex.EncodeToString(mac(secret, timestamp, nonce, body))
}

// CheckHeaders reports ErrBadRequest when a header has the wrong shape. It runs
// before the endpoint is loaded, so a malformed call costs no database read.
func CheckHeaders(timestamp, nonce, signature string) error {
	sent, ok := strings.CutPrefix(signature, "v1=")
	if !ok || !nonceRe.MatchString(nonce) {
		return ErrBadRequest
	}
	if _, err := hex.DecodeString(sent); err != nil {
		return ErrBadRequest
	}
	if _, err := strconv.ParseInt(timestamp, 10, 64); err != nil {
		return ErrBadRequest
	}
	return nil
}

// Verify checks header shape (ErrBadRequest), then the timestamp window and
// the signature against each candidate secret, current first (ErrUnauthorized).
// Every secret is tried so timing does not reveal which one matched.
func Verify(secrets [][]byte, now time.Time, timestamp, nonce, signature string, body []byte) error {
	sent, ok := strings.CutPrefix(signature, "v1=")
	if !ok || !nonceRe.MatchString(nonce) {
		return ErrBadRequest
	}
	sentMAC, err := hex.DecodeString(sent)
	if err != nil {
		return ErrBadRequest
	}
	sec, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrBadRequest
	}
	skew := now.Sub(time.Unix(sec, 0))
	inWindow := skew <= MaxSkew && skew >= -MaxSkew
	matched := false
	for _, secret := range secrets {
		if hmac.Equal(sentMAC, mac(secret, timestamp, nonce, body)) {
			matched = true
		}
	}
	if !inWindow || !matched {
		return ErrUnauthorized
	}
	return nil
}

type rawPayload struct {
	Issue  string `json:"issue"`
	Result Result `json:"result"`
	Ref    string `json:"ref"`
}

// ParsePayload strictly decodes the allowlisted body.
func ParsePayload(body []byte) (Payload, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var raw rawPayload
	if err := dec.Decode(&raw); err != nil {
		return Payload{}, ErrBadRequest
	}
	if _, err := dec.Token(); err != io.EOF {
		return Payload{}, ErrBadRequest
	}
	if !issueRe.MatchString(raw.Issue) ||
		(raw.Result != ResultSuccess && raw.Result != ResultFailure) ||
		(raw.Ref != "" && !refRe.MatchString(raw.Ref)) {
		return Payload{}, ErrBadRequest
	}
	return Payload(raw), nil
}
