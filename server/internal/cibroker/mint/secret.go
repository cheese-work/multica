package mint

import "log/slog"

// Secret holds a token. Every formatting, logging and JSON path prints
// [redacted]; Reveal is the only way out.
type Secret string

func (Secret) String() string               { return "[redacted]" }
func (Secret) GoString() string             { return "[redacted]" }
func (Secret) LogValue() slog.Value         { return slog.StringValue("[redacted]") }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func (Secret) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }

// Reveal returns the token value. Use it only to write the HTTP response or to
// build a GitHub request.
func (s Secret) Reveal() string { return string(s) }
