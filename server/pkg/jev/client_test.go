package jev

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient points a client at a stub server with retries disabled
// unless the test opts in.
func newTestClient(t *testing.T, handler http.HandlerFunc, mutate func(*Options)) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	opts := Options{APIKey: "test-key", BaseURL: srv.URL, RetryCount: -1}
	if mutate != nil {
		mutate(&opts)
	}
	c, err := NewClient(opts)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestNewClientRequiresAPIKey(t *testing.T) {
	if _, err := NewClient(Options{}); err == nil {
		t.Fatal("NewClient accepted an empty API key")
	}
}

func TestNewClientPinsExactModelVersion(t *testing.T) {
	c, err := NewClient(Options{APIKey: "k"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.Model() != DefaultModel {
		t.Errorf("Model() = %q, want %q", c.Model(), DefaultModel)
	}
	// A moving alias would re-tune every gated decision without a code
	// change, so the default must not be one.
	if c.Model() == "jev-latest" || c.Model() == "jev-preview" {
		t.Errorf("default model is a moving alias: %q", c.Model())
	}
}

// The request must match the documented wire shape exactly: state, model,
// and a questions map keyed by caller-chosen ids.
func TestEvaluateSendsDocumentedWireShape(t *testing.T) {
	var got struct {
		State     any                        `json:"state"`
		Model     string                     `json:"model"`
		Questions map[string]json.RawMessage `json:"questions"`
	}
	var auth string

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/systemone" {
			t.Errorf("path = %q, want /systemone", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writeJSON(w, http.StatusOK, `{"model":"jev-1.13.0","answers":{"is_urgent":{"type":"noul","noul":0.92}},"usage":{"input_tokens":312,"output_tokens":48}}`)
	}, nil)

	resp, err := c.Evaluate(context.Background(), Request{
		State: "Help! My payouts have been failing for 3 days.",
		Questions: map[string]Question{
			"is_urgent": NewNoul("Does this convey urgency?", nil),
		},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if auth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want bearer token", auth)
	}
	if got.Model != DefaultModel {
		t.Errorf("request model = %q, want %q", got.Model, DefaultModel)
	}
	if _, ok := got.Questions["is_urgent"]; !ok {
		t.Errorf("questions map lost the caller's id, got %v", got.Questions)
	}
	if resp.Model != "jev-1.13.0" {
		t.Errorf("response model = %q", resp.Model)
	}
	if resp.Usage.InputTokens != 312 {
		t.Errorf("input tokens = %d, want 312", resp.Usage.InputTokens)
	}
}

func TestEvaluateDecodesAllThreeAnswerTypes(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{
			"model": "jev-1.13.0",
			"answers": {
				"urgent": {"type":"noul","noul":0.92},
				"team":   {"type":"choice","choice":"technical","probabilities":{"billing":0.08,"technical":0.85,"sales":0.07},"confidence":0.82},
				"anger":  {"type":"score","score":1.6,"legend":{"0":"Calm","1":"Frustrated","2":"Very angry"},"probabilities":{"0":0.05,"1":0.3,"2":0.65},"confidence":0.78}
			},
			"usage": {"input_tokens": 312, "output_tokens": 48}
		}`)
	}, nil)

	resp, err := c.Evaluate(context.Background(), Request{
		State: "state",
		Questions: map[string]Question{
			"urgent": NewNoul("urgent?", nil),
			"team":   NewChoice("which team?", map[string]string{"billing": "b", "technical": "t", "sales": ""}),
			"anger":  NewScore("how angry?", []string{"Calm", "Frustrated", "Very angry"}),
		},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	urgent, err := mustAnswer(t, resp, "urgent").AsNoul()
	if err != nil || urgent != 0.92 {
		t.Errorf("AsNoul = (%v, %v), want (0.92, nil)", urgent, err)
	}
	team, teamConf, err := mustAnswer(t, resp, "team").AsChoice()
	if err != nil || team != "technical" || teamConf != 0.82 {
		t.Errorf("AsChoice = (%q, %v, %v)", team, teamConf, err)
	}
	anger, angerConf, err := mustAnswer(t, resp, "anger").AsScore()
	if err != nil || anger != 1.6 || angerConf != 0.78 {
		t.Errorf("AsScore = (%v, %v, %v)", anger, angerConf, err)
	}
	if legend := mustAnswer(t, resp, "anger").Legend["2"]; legend != "Very angry" {
		t.Errorf("legend[2] = %q, want Very angry", legend)
	}
}

// Reading an answer as the wrong type must fail loudly. Without the check a
// noul read as a choice would silently yield "" — a confident-looking empty
// decision.
func TestAnswerAccessorsRejectWrongType(t *testing.T) {
	noul := Answer{Type: TypeNoul, Noul: 0.92}
	if _, _, err := noul.AsChoice(); err == nil {
		t.Error("AsChoice accepted a noul answer")
	}
	if _, _, err := noul.AsScore(); err == nil {
		t.Error("AsScore accepted a noul answer")
	}
	choice := Answer{Type: TypeChoice, Choice: "technical", Confidence: 0.82}
	if _, err := choice.AsNoul(); err == nil {
		t.Error("AsNoul accepted a choice answer")
	}
}

// The gate is the kill switch: a denied call must never reach the network.
func TestEvaluateGateBlocksBeforeRequest(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusOK, `{"model":"jev-1.13.0","answers":{},"usage":{}}`)
	}, func(o *Options) {
		o.Gate = GateFunc(func(context.Context) bool { return false })
	})

	_, err := c.Evaluate(context.Background(), Request{
		State:     "state",
		Questions: map[string]Question{"q": NewNoul("q?", nil)},
	})
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("gate denied the call but %d request(s) still went out", n)
	}
}

// One malformed question would waste the round trip for every question
// sharing the request, so validation happens locally.
func TestEvaluateValidatesQuestionsBeforeSending(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusOK, `{"model":"jev-1.13.0","answers":{},"usage":{}}`)
	}, nil)

	for name, req := range map[string]Request{
		"no questions": {State: "s", Questions: map[string]Question{}},
		"one-option choice": {State: "s", Questions: map[string]Question{
			"ok":  NewNoul("fine?", nil),
			"bad": NewChoice("pick", map[string]string{"only": ""}),
		}},
		"one-level score": {State: "s", Questions: map[string]Question{
			"bad": NewScore("rate", []string{"Calm"}),
		}},
		"empty instructions": {State: "s", Questions: map[string]Question{
			"bad": NewNoul("", nil),
		}},
	} {
		if _, err := c.Evaluate(context.Background(), req); err == nil {
			t.Errorf("%s: Evaluate accepted an invalid request", name)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("invalid requests reached the network %d time(s)", n)
	}
}

func TestEvaluateRetriesRetryableStatuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantAll bool
	}{
		{"rate limited", http.StatusTooManyRequests, true},
		{"overloaded", StatusOverloaded, true},
		{"server error", http.StatusInternalServerError, true},
		{"unauthorized", http.StatusUnauthorized, false},
		{"validation", http.StatusUnprocessableEntity, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) < 3 {
					writeJSON(w, tc.status, `{"message":"try later"}`)
					return
				}
				writeJSON(w, http.StatusOK, `{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.5}},"usage":{}}`)
			}, func(o *Options) {
				o.RetryCount = DefaultRetryCount
				o.RetryWaitTime = time.Millisecond
				o.RetryMaxWaitTime = 5 * time.Millisecond
			})

			_, err := c.Evaluate(context.Background(), Request{
				State:     "s",
				Questions: map[string]Question{"q": NewNoul("q?", nil)},
			})

			if tc.wantAll {
				if err != nil {
					t.Fatalf("Evaluate: %v", err)
				}
				if n := calls.Load(); n != 3 {
					t.Errorf("attempts = %d, want 3", n)
				}
				return
			}
			if err == nil {
				t.Fatal("Evaluate succeeded on a non-retryable status")
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("non-retryable status retried: %d attempts", n)
			}
		})
	}
}

func TestEvaluateSurfacesTypedAPIError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusUnprocessableEntity, `{"message":"questions.bad.criteria: too few options"}`)
	}, nil)

	_, err := c.Evaluate(context.Background(), Request{
		State:     "s",
		Questions: map[string]Question{"q": NewNoul("q?", nil)},
	})

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %T (%v), want *APIError", err, err)
	}
	if !apiErr.IsInvalidRequest() {
		t.Errorf("IsInvalidRequest() = false for status %d", apiErr.HTTPStatus)
	}
	if apiErr.Retryable() {
		t.Error("422 reported as retryable")
	}
	if apiErr.Message != "questions.bad.criteria: too few options" {
		t.Errorf("Message = %q", apiErr.Message)
	}
}

func TestParseRetryAfterClampsAndIgnoresGarbage(t *testing.T) {
	maxWait := 5 * time.Second
	for name, tc := range map[string]struct {
		header string
		want   time.Duration
	}{
		"absent":     {"", 0},
		"http date":  {"Wed, 21 Oct 2026 07:28:00 GMT", 0},
		"negative":   {"-5", 0},
		"in range":   {"2", 2 * time.Second},
		"over clamp": {"600", maxWait},
	} {
		if got := parseRetryAfter(tc.header, maxWait); got != tc.want {
			t.Errorf("%s: parseRetryAfter(%q) = %v, want %v", name, tc.header, got, tc.want)
		}
	}
}

func mustAnswer(t *testing.T, resp *Response, id string) Answer {
	t.Helper()
	a, err := resp.Answer(id)
	if err != nil {
		t.Fatalf("Answer(%q): %v", id, err)
	}
	return a
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}
