package util

import (
	"net/http/httptest"
	"testing"
)

func TestRequestOrigin(t *testing.T) {
	cases := []struct{ name, origin, referer, want string }{
		{"origin header", "https://multica.congvc.dev", "", "https://multica.congvc.dev"},
		{"referer fallback", "", "https://multica.congvc.dev/ws/settings?tab=x", "https://multica.congvc.dev"},
		{"origin wins", "https://a.test", "https://b.test/x", "https://a.test"},
		{"opaque null origin", "null", "", ""},
		{"non-http scheme", "javascript:alert(1)", "", ""},
		{"none", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if c.referer != "" {
				r.Header.Set("Referer", c.referer)
			}
			if got := RequestOrigin(r); got != c.want {
				t.Fatalf("RequestOrigin = %q, want %q", got, c.want)
			}
		})
	}
}

func TestReturnOrigin(t *testing.T) {
	t.Setenv("CORS_ALLOWED_ORIGINS", "https://ts.test:19443, https://multica.congvc.dev/")
	const fb = "https://ts.test:19443"
	cases := []struct{ name, origin, want string }{
		{"allow-listed", "https://multica.congvc.dev", "https://multica.congvc.dev"},
		{"not listed", "https://evil.test", fb},
		{"prefix is not a match", "https://multica.congvc.dev.evil.test", fb},
		{"empty", "", fb},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ReturnOrigin(c.origin, fb); got != c.want {
				t.Fatalf("ReturnOrigin(%q) = %q, want %q", c.origin, got, c.want)
			}
		})
	}
	t.Setenv("CORS_ALLOWED_ORIGINS", "")
	if got := ReturnOrigin("https://multica.congvc.dev", fb); got != fb {
		t.Fatalf("unset allow-list: got %q, want fallback", got)
	}
}
