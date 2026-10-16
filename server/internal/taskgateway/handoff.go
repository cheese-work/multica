package taskgateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/url"
	"strings"

	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

const HandoffLimit = 1 << 20

type handoff struct {
	Binding credentialexec.Binding `json:"binding"`
	BaseURL string                 `json:"base_url"`
	Key     string                 `json:"key"`
}

func validHandoff(grant Grant, expected credentialexec.Binding) bool {
	if expected.Validate() != nil || grant.Binding != expected || grant.Key == "" || strings.ContainsAny(grant.Key, "\r\n\x00") {
		return false
	}
	endpoint, err := url.Parse(grant.BaseURL)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.Opaque != "" || endpoint.Path != "" && endpoint.Path != "/" {
		return false
	}
	if endpoint.Scheme == "https" {
		return true
	}
	address := net.ParseIP(endpoint.Hostname())
	return endpoint.Scheme == "http" && address != nil && address.IsLoopback()
}

func EncodeHandoff(grant Grant) ([]byte, error) {
	if !validHandoff(grant, grant.Binding) {
		return nil, ErrUnavailable
	}
	contents, err := json.Marshal(handoff(grant))
	if err != nil || len(contents) > HandoffLimit {
		return nil, ErrUnavailable
	}
	return contents, nil
}

func DecodeHandoff(contents []byte, expected credentialexec.Binding) (Grant, error) {
	if len(contents) > HandoffLimit {
		return Grant{}, ErrUnavailable
	}
	fields := json.NewDecoder(bytes.NewReader(contents))
	if validateJSONFields(fields, 0) != nil {
		return Grant{}, ErrUnavailable
	}
	if _, err := fields.Token(); err != io.EOF {
		return Grant{}, ErrUnavailable
	}
	var decoded handoff
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&decoded) != nil {
		return Grant{}, ErrUnavailable
	}
	grant := Grant(decoded)
	if !validHandoff(grant, expected) {
		return Grant{}, ErrUnavailable
	}
	return grant, nil
}
