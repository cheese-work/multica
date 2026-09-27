package jev

import (
	"errors"
	"fmt"
	"net/http"
)

// StatusOverloaded is TypeSafe's non-standard "temporarily overloaded"
// status. It is retryable on the same terms as 429.
const StatusOverloaded = 529

// ErrDisabled is returned by [Client.Evaluate] when the client's [Gate]
// reports that Jev calls are not permitted for this request. It is not a
// failure: it is the kill switch working, and callers must fall back to
// their non-Jev path rather than surfacing an error.
var ErrDisabled = errors.New("jev: evaluation disabled by gate")

// ErrNoQuestions is returned when a request carries an empty question map.
// The API would reject it with 422; failing locally saves the round trip.
var ErrNoQuestions = errors.New("jev: request has no questions")

// APIError is returned when TypeSafe responds with a non-2xx status.
//
// The documented error statuses are 401 (bad key), 422 (validation), 429
// (rate limit), and 529 (overloaded). Provider messages and bodies are not
// retained, because upstream error text is untrusted and may contain secrets.
type APIError struct {
	HTTPStatus int    `json:"-"`
	Message    string `json:"message,omitempty"`
	Type       string `json:"type,omitempty"`
	RawBody    []byte `json:"-"`
}

// Error implements error.
func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	msg := http.StatusText(e.HTTPStatus)
	if msg == "" {
		msg = "unknown error"
	}
	return fmt.Sprintf("jev: %d %s", e.HTTPStatus, msg)
}

// IsUnauthorized reports an HTTP 401 — a missing or invalid API key. It is
// never retryable.
func (e *APIError) IsUnauthorized() bool {
	return e != nil && e.HTTPStatus == http.StatusUnauthorized
}

// IsInvalidRequest reports an HTTP 422 — the request body failed validation.
// Retrying an unchanged body cannot help.
func (e *APIError) IsInvalidRequest() bool {
	return e != nil && e.HTTPStatus == http.StatusUnprocessableEntity
}

// IsRateLimited reports an HTTP 429. Either the token-per-second or the
// request-per-minute limit was crossed.
func (e *APIError) IsRateLimited() bool {
	return e != nil && e.HTTPStatus == http.StatusTooManyRequests
}

// IsOverloaded reports an HTTP 529.
func (e *APIError) IsOverloaded() bool {
	return e != nil && e.HTTPStatus == StatusOverloaded
}

// Retryable reports whether re-sending the same request could succeed.
func (e *APIError) Retryable() bool {
	if e == nil {
		return false
	}
	return retryableStatus(e.HTTPStatus)
}

// retryableStatus is the single definition of "worth retrying", shared by
// the resty retry condition and by [APIError.Retryable] so the two can never
// drift apart.
func retryableStatus(status int) bool {
	switch {
	case status == http.StatusTooManyRequests, status == StatusOverloaded:
		return true
	case status >= 500 && status <= 599:
		return true
	default:
		return false
	}
}

func parseAPIError(status int, _ []byte) *APIError {
	return &APIError{HTTPStatus: status}
}
