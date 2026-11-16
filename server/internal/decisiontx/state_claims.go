package decisiontx

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// askNow is the clock for observed_at checks; tests replace it.
var askNow = time.Now

const (
	// maxClaimAge is how old a read-back may be when the ask is sent.
	maxClaimAge = 2 * time.Hour
	// claimClockSkew tolerates a sender clock slightly ahead of the server.
	claimClockSkew = 5 * time.Minute
)

// StateClaim is one statement about live state the ask relies on, with the
// command that reads it back and when the sender ran that command (CHE-5950).
// The server checks shape and time, never that the claim is true or complete.
type StateClaim struct {
	Claim           string `json:"claim"`
	ReadbackCommand string `json:"readback_command"`
	ObservedAt      string `json:"observed_at"`
}

var claimFields = []string{"claim", "readback_command", "observed_at"}

func decodeStateClaims(raw json.RawMessage, into *[]StateClaim) error {
	if len(raw) == 0 {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return &AskError{"ask.state_claims", "must be a list"}
	}
	for index, item := range items {
		prefix := fmt.Sprintf("ask.state_claims[%d]", index)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil || fields == nil {
			return &AskError{prefix, "must be an object with claim, readback_command, observed_at"}
		}
		if err := rejectUnknown(prefix, fields, claimFields); err != nil {
			return err
		}
		var claim StateClaim
		for _, field := range []struct {
			name string
			into *string
		}{{"claim", &claim.Claim}, {"readback_command", &claim.ReadbackCommand}, {"observed_at", &claim.ObservedAt}} {
			if err := decodeString(prefix+"."+field.name, fields[field.name], field.into); err != nil {
				return err
			}
		}
		*into = append(*into, claim)
	}
	return nil
}

// validateStateClaims runs at the agent-input boundary (the decoders), so asks
// the server builds itself are not held to it.
func (ask Ask) validateStateClaims() error {
	if len(ask.StateClaims) == 0 {
		return &AskError{"ask.state_claims", "requires at least one item"}
	}
	now := askNow()
	for index, claim := range ask.StateClaims {
		prefix := fmt.Sprintf("ask.state_claims[%d]", index)
		if blank(claim.Claim) {
			return &AskError{prefix + ".claim", "is required"}
		}
		if blank(claim.ReadbackCommand) {
			return &AskError{prefix + ".readback_command", "is required"}
		}
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(claim.ObservedAt))
		if err != nil {
			return &AskError{prefix + ".observed_at", "must be an RFC 3339 time with an offset"}
		}
		if at.After(now.Add(claimClockSkew)) {
			return &AskError{prefix + ".observed_at", "is in the future"}
		}
		if now.Sub(at) > maxClaimAge {
			return &AskError{prefix + ".observed_at", "is older than 2h; read the state again"}
		}
	}
	return nil
}

func (ask Ask) stateClaimsContent() string {
	if len(ask.StateClaims) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString("**State read before sending**\n")
	for _, claim := range ask.StateClaims {
		fmt.Fprintf(&out, "- %s · `%s` · read at %s\n", claim.Claim, claim.ReadbackCommand, claim.ObservedAt)
	}
	return out.String() + "\n"
}
