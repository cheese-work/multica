package credentialexec

import (
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func ValidateInputs(inputs map[string][]byte) error {
	if len(inputs) > 128 {
		return ErrUnavailable
	}
	total := 0
	for name, contents := range inputs {
		if !utf8.ValidString(name) || len(name) > 1024 || !strings.HasPrefix(name, "multica-input/") || !filepath.IsLocal(name) || filepath.ToSlash(filepath.Clean(name)) != name || strings.ContainsAny(name, "\\\x00") || len(contents) > 1<<20 {
			return ErrUnavailable
		}
		for _, character := range name {
			if character < 32 || character == 127 {
				return ErrUnavailable
			}
		}
		parts := strings.Split(name, "/")
		if len(parts) > 16 {
			return ErrUnavailable
		}
		for _, component := range parts {
			if component == "" || len(component) > 255 {
				return ErrUnavailable
			}
		}
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			if _, exists := inputs[parent]; exists {
				return ErrUnavailable
			}
		}
		total += len(contents)
		if total > 8<<20 {
			return ErrUnavailable
		}
	}
	return nil
}
