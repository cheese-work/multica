// Package doopread reads design attestations from doop over its service route.
package doopread

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	// Timeout bounds one read, headers and body.
	Timeout = 3 * time.Second
	// MaxBody caps the response body.
	MaxBody = 256 << 10
	// MinTokenLen is the shortest shared token the channel accepts (plan D1.7).
	MinTokenLen = 32

	envURL   = "MULTICA_DOOP_API_URL"
	envToken = "MULTICA_DOOP_ATTESTATION_TOKEN"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

var (
	ErrNotConfigured = errors.New("doop attestation reader is not configured")
	ErrNotFound      = errors.New("attestation not found")
	ErrUnavailable   = errors.New("doop is unavailable")
)

// Author is one recorded writer of the attested page.
type Author struct {
	Kind     string   `json:"kind"`
	AgentID  *string  `json:"agentId"`
	TaskID   *string  `json:"taskId"`
	UserID   *string  `json:"userId"`
	Provider *string  `json:"provider"`
	Model    *string  `json:"model"`
	Sources  []string `json:"sources"`
}

// Frame is an attested frame and the version the run saw.
type Frame struct {
	FrameID   string `json:"frameId"`
	UpdatedAt int64  `json:"updatedAt"`
}

// View is the attestation view.
type View struct {
	CanvasID           string
	PageID             string
	PageIssueID        string // empty when the page is bound to no issue
	Ruleset            string // the design ruleset the run applied; empty when doop names none
	Complete           bool
	Current            bool
	Frames             []Frame
	Authors            []Author
	ProvenanceComplete bool
}

// Config is the channel to doop.
type Config struct {
	BaseURL string
	Token   string
	// AllowLoopbackHTTP lets tests talk to an httptest server.
	AllowLoopbackHTTP bool
}

// Reader reads attestations. A nil Reader is not configured.
type Reader struct {
	base   string
	token  string
	client *http.Client
}

// New returns a reader. A config that is not usable gives a reader whose reads
// return ErrNotConfigured: a base URL that is not an https origin (plain http
// only for a loopback host and only with AllowLoopbackHTTP), or a token shorter
// than MinTokenLen.
func New(cfg Config) *Reader {
	r := &Reader{}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return r
	}
	secure := u.Scheme == "https"
	loopback := u.Scheme == "http" && cfg.AllowLoopbackHTTP && isLoopback(u.Hostname())
	if !secure && !loopback || len(cfg.Token) < MinTokenLen {
		return r
	}
	r.base = u.Scheme + "://" + u.Host
	r.token = cfg.Token
	r.client = &http.Client{
		Timeout:       Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return r
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// FromEnv builds a reader from MULTICA_DOOP_API_URL and MULTICA_DOOP_ATTESTATION_TOKEN.
// Unset, it is not configured.
func FromEnv() *Reader {
	return New(Config{BaseURL: os.Getenv(envURL), Token: os.Getenv(envToken)})
}

// attestationWire is the doop view. Every field the guard reads must be present;
// fields it does not read are ignored so doop can add to the view.
type attestationWire struct {
	ID          *string         `json:"id"`
	CanvasID    *string         `json:"canvasId"`
	PageID      *string         `json:"pageId"`
	PageIssueID json.RawMessage `json:"pageIssueId"`
	Ruleset     *string         `json:"ruleset"`
	Complete    *bool           `json:"complete"`
	Current     *bool           `json:"current"`
	Frames      *[]frameWire    `json:"frames"`
	Authors     *[]Author       `json:"authors"`
	Provenance  *bool           `json:"provenanceComplete"`
}

type frameWire struct {
	FrameID   *string `json:"frameId"`
	UpdatedAt *int64  `json:"updatedAt"`
}

// Attestation reads one attestation by run id. An id that is not
// ^[A-Za-z0-9_-]{1,64}$ is ErrNotFound and makes no call. Besides a 404, every
// failure (transport, timeout, redirect, a status other than 200, an oversize
// or malformed body) is ErrUnavailable.
func (r *Reader) Attestation(ctx context.Context, id string) (View, error) {
	if r == nil || r.client == nil {
		return View{}, ErrNotConfigured
	}
	if !idPattern.MatchString(id) {
		return View{}, ErrNotFound
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+"/api/service/design-attestations/"+id, nil)
	if err != nil {
		return View{}, ErrUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return View{}, ErrUnavailable
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return View{}, ErrNotFound
	default:
		return View{}, ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil || len(raw) > MaxBody {
		return View{}, ErrUnavailable
	}
	view, ok := decode(raw, id)
	if !ok {
		return View{}, ErrUnavailable
	}
	return view, nil
}

func decode(raw []byte, id string) (View, bool) {
	var wire attestationWire
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&wire); err != nil {
		return View{}, false
	}
	if _, err := dec.Token(); err != io.EOF { // one JSON value only
		return View{}, false
	}
	if wire.CanvasID == nil || *wire.CanvasID == "" || wire.PageID == nil || *wire.PageID == "" ||
		wire.Complete == nil || wire.Current == nil || wire.Frames == nil || wire.Authors == nil || wire.Provenance == nil ||
		wire.ID != nil && *wire.ID != id {
		return View{}, false
	}
	view := View{CanvasID: *wire.CanvasID, PageID: *wire.PageID, Complete: *wire.Complete, Current: *wire.Current,
		Authors: *wire.Authors, ProvenanceComplete: *wire.Provenance, Frames: make([]Frame, 0, len(*wire.Frames))}
	// pageIssueId is a string or null, and the key must be there.
	switch s := strings.TrimSpace(string(wire.PageIssueID)); {
	case s == "":
		return View{}, false
	case s != "null":
		if json.Unmarshal(wire.PageIssueID, &view.PageIssueID) != nil {
			return View{}, false
		}
	}
	if wire.Ruleset != nil {
		view.Ruleset = *wire.Ruleset
	}
	for _, frame := range *wire.Frames {
		if frame.FrameID == nil || *frame.FrameID == "" || frame.UpdatedAt == nil {
			return View{}, false
		}
		view.Frames = append(view.Frames, Frame{FrameID: *frame.FrameID, UpdatedAt: *frame.UpdatedAt})
	}
	for _, author := range view.Authors {
		if author.Kind == "" {
			return View{}, false
		}
	}
	return view, true
}
