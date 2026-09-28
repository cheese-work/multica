package jev

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
)

// DefaultBaseURL is the TypeSafe API root.
const DefaultBaseURL = "https://api.typesafe.ai/v1"

// DefaultModel pins an exact model version rather than the `jev-latest`
// alias. A moving alias would silently re-tune every gated decision in the
// product; upgrades are a deliberate change with their own evaluation.
const DefaultModel = "jev-1.13.0"

// DefaultUserAgent identifies this client to TypeSafe.
const DefaultUserAgent = "multica-jev-go/0.1"

// DefaultTimeout bounds one evaluation. System One answers in single-digit
// seconds; a request still running well past that is not going to be useful
// to the request that is waiting on it.
const DefaultTimeout = 15 * time.Second

// Default retry policy. Retries cost nothing but latency: input is billed
// once per accepted request, and a 429 or 529 was never accepted.
const (
	DefaultRetryCount       = 3
	DefaultRetryWaitTime    = 500 * time.Millisecond
	DefaultRetryMaxWaitTime = 5 * time.Second
)

// Gate decides whether a Jev call is permitted for a given request. It is
// the kill switch: every call goes through it, so the feature flag cannot be
// bypassed by a new call site forgetting to check one.
//
// It is an interface rather than a concrete feature-flag dependency to keep
// this package free of Multica imports. Wiring passes an implementation
// backed by the jev_enabled flag.
type Gate interface {
	// Allowed reports whether this context may spend a Jev call.
	Allowed(ctx context.Context) bool
}

// GateFunc adapts a function to [Gate].
type GateFunc func(ctx context.Context) bool

// Allowed implements [Gate].
func (f GateFunc) Allowed(ctx context.Context) bool { return f(ctx) }

// Options configures a [Client]. Only APIKey is required.
type Options struct {
	// APIKey is the TypeSafe key, sent as a bearer token.
	APIKey string

	// BaseURL overrides the API root. Defaults to [DefaultBaseURL].
	BaseURL string

	// Model is the model or alias sent with every request. Defaults to
	// [DefaultModel].
	Model string

	// UserAgent defaults to [DefaultUserAgent].
	UserAgent string

	// Timeout is the per-evaluation timeout, including retries. Zero means
	// [DefaultTimeout]; values above the transport maximum are clamped.
	Timeout time.Duration

	// Gate is the kill switch consulted before every call. A nil Gate
	// FAILS OPEN — it permits every call — which is the right default for
	// tests but wrong for production. The Multica-side adapter
	// (featureflags.JevGate / featureflags.JevProductionGate) deliberately
	// has the OPPOSITE polarity: a nil *featureflag.Service inside that
	// adapter fails closed. So production wiring must always set a
	// non-nil Gate here — relying on this field's own nil behavior would
	// permit every call instead of denying it.
	Gate Gate

	// HTTPClient contributes its TLS roots, cookie jar, and a shorter timeout.
	// Its proxy, dialer, and redirect behavior are replaced by the egress policy.
	HTTPClient *http.Client

	// EgressAllowlist adds exact host:port entries for code-controlled service
	// endpoints. The deployment environment allowlist is always applied too.
	EgressAllowlist []string

	// RetryCount, RetryWaitTime, and RetryMaxWaitTime override the default
	// retry policy. A negative RetryCount disables retries.
	RetryCount       int
	RetryWaitTime    time.Duration
	RetryMaxWaitTime time.Duration

	lookupIP    ipLookupFunc
	dialContext dialContextFunc
}

// Client is the TypeSafe System One client. It is safe for concurrent use.
type Client struct {
	rc      *resty.Client
	baseURL string
	model   string
	gate    Gate
	timeout time.Duration
}

type discardRestyLogger struct{}

func (discardRestyLogger) Errorf(string, ...interface{}) {}
func (discardRestyLogger) Warnf(string, ...interface{})  {}
func (discardRestyLogger) Debugf(string, ...interface{}) {}

// NewClient constructs a Client, failing on obviously broken options.
func NewClient(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, errors.New("jev: APIKey is required")
	}

	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	baseURL, authority, err := normalizeBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	allowed, err := loadEgressAllowlist(os.Getenv(EgressAllowlistEnv), opts.EgressAllowlist)
	if err != nil {
		return nil, err
	}
	if _, ok := allowed[authority]; !ok {
		return nil, ErrEgressDenied
	}

	model := opts.Model
	if model == "" {
		model = DefaultModel
	}

	userAgent := opts.UserAgent
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}

	timeout := opts.Timeout
	if timeout < 0 {
		return nil, errors.New("jev: timeout must be positive")
	}
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	if timeout > maxRequestTimeout {
		timeout = maxRequestTimeout
	}
	if opts.HTTPClient != nil && opts.HTTPClient.Timeout > 0 && opts.HTTPClient.Timeout < timeout {
		timeout = opts.HTTPClient.Timeout
	}
	httpClient, err := newEgressHTTPClient(opts.HTTPClient, allowed, timeout, opts.lookupIP, opts.dialContext)
	if err != nil {
		return nil, err
	}

	rc := resty.NewWithClient(httpClient).
		SetLogger(discardRestyLogger{}).
		SetResponseBodyLimit(maxResponseBytes).
		SetBaseURL(baseURL).
		SetHeader("Content-Type", "application/json").
		SetHeader("Accept", "application/json").
		SetHeader("User-Agent", userAgent).
		SetAuthToken(opts.APIKey).
		SetTimeout(timeout)

	applyRetryPolicy(rc, opts)

	return &Client{rc: rc, baseURL: baseURL, model: model, gate: opts.Gate, timeout: timeout}, nil
}

// applyRetryPolicy wires exponential backoff for the statuses TypeSafe
// documents as retryable, honoring a Retry-After header when one is sent.
func applyRetryPolicy(rc *resty.Client, opts Options) {
	count := opts.RetryCount
	if count == 0 {
		count = DefaultRetryCount
	}
	if count < 0 {
		return
	}

	wait := opts.RetryWaitTime
	if wait <= 0 {
		wait = DefaultRetryWaitTime
	}
	maxWait := opts.RetryMaxWaitTime
	if maxWait <= 0 {
		maxWait = DefaultRetryMaxWaitTime
	}

	rc.SetRetryCount(count).
		SetRetryWaitTime(wait).
		SetRetryMaxWaitTime(maxWait).
		AddRetryCondition(func(resp *resty.Response, err error) bool {
			if err != nil {
				if errors.Is(err, ErrEgressDenied) || errors.Is(err, ErrResponseTooLarge) || errors.Is(err, resty.ErrResponseBodyTooLarge) {
					return false
				}
				return true
			}
			return retryableStatus(resp.StatusCode())
		}).
		SetRetryAfter(func(_ *resty.Client, resp *resty.Response) (time.Duration, error) {
			// Returning 0 with a nil error tells resty to fall back to its
			// own backoff, which is what we want when the server does not
			// name a delay.
			if resp == nil {
				return 0, nil
			}
			return parseRetryAfter(resp.Header().Get("Retry-After"), maxWait), nil
		})
}

// parseRetryAfter reads the delay-seconds form of a Retry-After header.
// TypeSafe does not document sending one, so an absent or unparsable header
// is not an error: it just means "use the default backoff". The value is
// clamped to maxWait so a hostile or mistaken header cannot park a request
// for minutes.
func parseRetryAfter(header string, maxWait time.Duration) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	seconds, err := strconv.Atoi(header)
	if err != nil || seconds <= 0 {
		return 0
	}
	delay := time.Duration(seconds) * time.Second
	if delay > maxWait {
		return maxWait
	}
	return delay
}

// Request is one evaluation: a state and a map of typed questions.
type Request struct {
	// State is the content to evaluate: a string, or structured data.
	//
	// Whatever goes in here is read literally and is NOT treated as hostile
	// by the model. Instructions embedded in it can move the answer, so a
	// state carrying untrusted text needs a provenance decision upstream.
	State any `json:"state"`

	// Model overrides the client's pinned model for this request.
	Model string `json:"model"`

	// Questions are keyed by ids you choose; answers come back under the
	// same keys. The keys are not sent to the model and do not affect
	// inference, so they are free to be descriptive.
	Questions map[string]Question `json:"questions"`
}

// Usage is the billed token count for a request. Output is free.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Response is one evaluation's result.
type Response struct {
	// Model is the model that actually served the request. Log it beside
	// every decision: it is the only way to tell later which model version
	// produced a given answer.
	Model string `json:"model"`

	// Answers holds one answer per question id.
	Answers map[string]Answer `json:"answers"`

	// Usage is the billed token count.
	Usage Usage `json:"usage"`
}

// Answer returns the answer for a question id.
func (r *Response) Answer(id string) (Answer, error) {
	if r == nil {
		return Answer{}, fmt.Errorf("jev: no response")
	}
	answer, ok := r.Answers[id]
	if !ok {
		return Answer{}, fmt.Errorf("jev: no answer for question %q", id)
	}
	return answer, nil
}

// Evaluate sends one state and its questions to System One.
//
// It returns [ErrDisabled] without making a request when the client's [Gate]
// denies the call — that is the kill switch working, not a failure, and the
// caller must fall back to its non-Jev path.
//
// Questions are validated locally first: the whole map travels in one
// request, so one malformed question would otherwise waste the round trip
// for all of them.
func (c *Client) Evaluate(ctx context.Context, req Request) (*Response, error) {
	if c.gate != nil && !c.gate.Allowed(ctx) {
		return nil, ErrDisabled
	}
	if len(req.Questions) == 0 {
		return nil, ErrNoQuestions
	}
	for id, question := range req.Questions {
		if err := question.Validate(); err != nil {
			return nil, fmt.Errorf("jev: question %q: %w", id, err)
		}
	}
	if req.Model == "" {
		req.Model = c.model
	}
	requestContext, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	out := &Response{}
	resp, err := c.rc.R().
		SetContext(requestContext).
		SetBody(req).
		SetResult(out).
		Post("/systemone")
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrEgressDenied) {
			return nil, ErrEgressDenied
		}
		if errors.Is(err, ErrResponseTooLarge) || errors.Is(err, resty.ErrResponseBodyTooLarge) {
			return nil, ErrResponseTooLarge
		}
		return nil, ErrUpstreamRequest
	}
	if resp.StatusCode() < http.StatusOK || resp.StatusCode() >= http.StatusMultipleChoices {
		return nil, parseAPIError(resp.StatusCode(), resp.Body())
	}
	return out, nil
}

// Model returns the model this client sends by default.
func (c *Client) Model() string { return c.model }

// BaseURL returns the resolved API root.
func (c *Client) BaseURL() string { return c.baseURL }
