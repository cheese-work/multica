// Package designguard holds the pure parts of the G4 design-card guard
// (CHE-7064 r3): the workspace setting, the doop URL grammar and the link test.
package designguard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Refusal codes of the URL grammar (plan D1.3).
const (
	CodeDesignFrameURLInvalid = "design_frame_url_invalid"
	CodeDesignURLMissing      = "design_url_missing"
	CodeDesignURLAmbiguous    = "design_url_ambiguous"
)

// SettingKey is the workspace settings key of the guard.
const SettingKey = "design_card_guard"

// Modes of the guard.
const (
	ModeOff   = "off"
	ModeWarn  = "warn"
	ModeBlock = "block"
)

// Refusal is a refusal with a stable code.
type Refusal struct{ Code, Message string }

func (r *Refusal) Error() string { return r.Code + ": " + r.Message }

// Settings is the parsed design_card_guard setting.
type Settings struct {
	Mode                string
	DoopOrigin          string
	DesignURLPropertyID string
	DesignWorkflowID    string
}

// FrameRef is a parsed doop link: canvas, plus the page and frame it names.
type FrameRef struct{ Canvas, Page, Frame string }

var (
	idPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	pathPattern = regexp.MustCompile(`^/c/([A-Za-z0-9_-]{1,64})(?:/proto)?$`)
	// linkCandidate finds the start of an absolute link; HasDoopLink trims and parses it.
	linkCandidate = regexp.MustCompile(`(?i)https?://[^\s<>()\[\]"'` + "`" + `]+`)
)

// ParseSettings reads design_card_guard from a workspace.settings JSON value.
// An absent key, or an object without a mode, is off. A malformed value is an
// error: callers must not guess. With the mode warn or block the origin and the
// Design url property id are required.
func ParseSettings(raw []byte) (Settings, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return Settings{Mode: ModeOff}, nil
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return Settings{}, fmt.Errorf("workspace settings: %w", err)
	}
	value, ok := all[SettingKey]
	if !ok {
		return Settings{Mode: ModeOff}, nil
	}
	var in struct {
		Mode                json.RawMessage `json:"mode"`
		DoopOrigin          *string         `json:"doop_origin"`
		DesignURLPropertyID *string         `json:"design_url_property_id"`
		DesignWorkflowID    *string         `json:"design_workflow_id"`
	}
	dec := json.NewDecoder(bytes.NewReader(value))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return Settings{}, fmt.Errorf("%s must be an object with mode, doop_origin, design_url_property_id and design_workflow_id", SettingKey)
	}
	out := Settings{Mode: ModeOff}
	if in.Mode != nil { // null is not a mode
		var mode *string
		if err := json.Unmarshal(in.Mode, &mode); err != nil || mode == nil || *mode != ModeOff && *mode != ModeWarn && *mode != ModeBlock {
			return Settings{}, fmt.Errorf("%s.mode must be off, warn or block", SettingKey)
		}
		out.Mode = *mode
	}
	if in.DoopOrigin != nil {
		origin, err := normalizeOrigin(*in.DoopOrigin)
		if err != nil {
			return Settings{}, fmt.Errorf("%s.doop_origin %w", SettingKey, err)
		}
		out.DoopOrigin = origin
	}
	for name, field := range map[string]struct {
		in  *string
		out *string
	}{
		"design_url_property_id": {in.DesignURLPropertyID, &out.DesignURLPropertyID},
		"design_workflow_id":     {in.DesignWorkflowID, &out.DesignWorkflowID},
	} {
		if field.in == nil {
			continue
		}
		if !uuidPattern.MatchString(*field.in) {
			return Settings{}, fmt.Errorf("%s.%s must be a UUID", SettingKey, name)
		}
		*field.out = strings.ToLower(*field.in)
	}
	if out.Mode != ModeOff && (out.DoopOrigin == "" || out.DesignURLPropertyID == "") {
		return Settings{}, fmt.Errorf("%s needs doop_origin and design_url_property_id in mode %s", SettingKey, out.Mode)
	}
	return out, nil
}

// normalizeOrigin returns scheme://host[:port] for an https origin with no
// userinfo, path, query or fragment.
func normalizeOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || strings.Contains(raw, "?") || strings.Contains(raw, "#") {
		return "", errors.New("must be an https origin such as https://doop.example.com")
	}
	return "https://" + strings.ToLower(u.Host), nil
}

// parseLink parses one link on origin into canvas, page and frame. A repeated
// frame or page value is ambiguous. ok is false for anything that is not a
// canvas link on origin.
func parseLink(origin, raw string) (ref FrameRef, ambiguous, ok bool) {
	want, err := normalizeOrigin(origin)
	if err != nil {
		return FrameRef{}, false, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || strings.ToLower(u.Scheme+"://"+u.Host) != want || u.Opaque != "" {
		return FrameRef{}, false, false
	}
	m := pathPattern.FindStringSubmatch(u.EscapedPath())
	if m == nil {
		return FrameRef{}, false, false
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return FrameRef{}, false, false
	}
	one := func(key string) (string, bool, bool) {
		values := query[key]
		switch {
		case len(values) == 0:
			return "", false, true
		case len(values) > 1:
			return "", true, true
		case !idPattern.MatchString(values[0]):
			return "", false, false
		}
		return values[0], false, true
	}
	frame, frameAmbiguous, frameOK := one("frame")
	page, pageAmbiguous, pageOK := one("page")
	if frameAmbiguous || pageAmbiguous {
		return FrameRef{}, true, true
	}
	if !frameOK || !pageOK {
		return FrameRef{}, false, false
	}
	return FrameRef{Canvas: m[1], Page: page, Frame: frame}, false, true
}

// ParseFrameURL parses an option frame_url: exactly one frame, at most one page.
func ParseFrameURL(origin, raw string) (FrameRef, error) {
	ref, ambiguous, ok := parseLink(origin, strings.TrimSpace(raw))
	switch {
	case ambiguous:
		return FrameRef{}, &Refusal{CodeDesignURLAmbiguous, "frame_url repeats frame or page"}
	case !ok || ref.Frame == "":
		return FrameRef{}, &Refusal{CodeDesignFrameURLInvalid, "frame_url must be <doop origin>/c/<canvas>?frame=<frame>"}
	}
	return ref, nil
}

// ParseDesignURL parses the issue Design url: at most one page, at most one
// frame, and at least one of the two. A value that is empty, off the origin or
// names neither a page nor a frame counts as missing.
func ParseDesignURL(origin, raw string) (FrameRef, error) {
	ref, ambiguous, ok := parseLink(origin, strings.TrimSpace(raw))
	switch {
	case ambiguous:
		return FrameRef{}, &Refusal{CodeDesignURLAmbiguous, "Design url repeats frame or page"}
	case !ok || ref.Frame == "" && ref.Page == "":
		return FrameRef{}, &Refusal{CodeDesignURLMissing, "the issue Design url must be a doop link with a page or a frame"}
	}
	return ref, nil
}

// HasDoopLink reports whether text holds a link on the doop origin (plan D1.2
// T3). It is a structural match on the URL host and port, not a word match
// (CHE-3345), and it ignores the path, the query and the scheme: a link that is
// malformed or points outside /c/ must still trigger the guard, never skip it.
func HasDoopLink(origin, text string) bool {
	normalized, err := normalizeOrigin(origin)
	if err != nil {
		return false
	}
	want, _ := url.Parse(normalized)
	for _, candidate := range linkCandidate.FindAllString(text, -1) {
		u, err := url.Parse(strings.TrimRight(candidate, ".,;:!?*_"))
		if err != nil || u.Hostname() == "" {
			continue
		}
		if strings.EqualFold(u.Hostname(), want.Hostname()) && effectivePort(u) == effectivePort(want) {
			return true
		}
	}
	return false
}

// effectivePort is the port of u, or 443 when it names none.
func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	return "443"
}
