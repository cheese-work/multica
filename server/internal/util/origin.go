package util

import (
	"net/http"
	"net/url"
	"os"
	"strings"
)

// RequestOrigin returns the http(s) origin (scheme://host) that started the
// request: the Origin header, else the Referer's origin (same-origin GETs send
// no Origin). Returns "" when neither is a usable http(s) origin.
func RequestOrigin(r *http.Request) string {
	raw := r.Header.Get("Origin")
	if raw == "" {
		raw = r.Header.Get("Referer")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// ReturnOrigin picks where an OAuth flow sends the browser back to: origin if
// it is an exact entry of CORS_ALLOWED_ORIGINS, else fallback. The allow-list
// check is what stops a signed-but-attacker-chosen origin becoming an open
// redirect.
func ReturnOrigin(origin, fallback string) string {
	if origin == "" {
		return fallback
	}
	for _, o := range strings.Split(os.Getenv("CORS_ALLOWED_ORIGINS"), ",") {
		if strings.TrimRight(strings.TrimSpace(o), "/") == origin {
			return origin
		}
	}
	return fallback
}
