package buildinfo

import (
	"regexp"
	"strings"
)

var stampPattern = regexp.MustCompile(`^(v?\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)-(\d{8}-\d{4})$`)

func Split(version string) (string, string) {
	plain, _, _ := strings.Cut(strings.TrimSpace(version), "+")
	if parts := stampPattern.FindStringSubmatch(plain); parts != nil {
		return parts[1], parts[2]
	}
	return plain, ""
}
