package browserlease

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

// Class-3 deny list (plan W1 r5, "Class-3 deny list"; CHE-4782, W3-19). The file is the gateway
// repo's class3-deny.txt. Mint reads only its [hosts] part: the [patterns] part is navigation
// filtering, which the gateway enforces.

var (
	// ErrClass3Denied: a host of the profile intersects the host part of the list.
	ErrClass3Denied = errors.New("the profile reaches a class-3 host, so no agent lease can use it")
	// ErrClass3Unchecked: the check cannot run, so the lease is refused (fail closed).
	ErrClass3Unchecked = errors.New("the profile's hosts cannot be checked against the class-3 list")
)

// Class3Deny is the host part of class3-deny.txt.
type Class3Deny struct{ hosts []string }

// ParseClass3Deny reads the [hosts] section. A file with no host fails: an empty list would
// deny nothing and look like a working check.
func ParseClass3Deny(r io.Reader) (*Class3Deny, error) {
	d := &Class3Deny{}
	inHosts := false
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "["):
			inHosts = line == "[hosts]"
		case inHosts:
			if h := normHost(line); h != "" {
				d.hosts = append(d.hosts, h)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(d.hosts) == 0 {
		return nil, errors.New("class3-deny: no [hosts] entries")
	}
	return d, nil
}

// LoadClass3Deny reads the list from a file.
func LoadClass3Deny(path string) (*Class3Deny, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	d, err := ParseClass3Deny(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return d, nil
}

// Match returns the listed host that intersects one of hosts. Two hosts intersect when they are
// equal or one is a subdomain of the other: the listed `paypal.com` covers `www.paypal.com`,
// and a profile for `google.com` can reach the listed `mail.google.com`.
func (d *Class3Deny) Match(hosts []string) (string, bool) {
	for _, h := range hosts {
		h = normHost(h)
		if h == "" {
			continue
		}
		for _, deny := range d.hosts {
			if h == deny || strings.HasSuffix(h, "."+deny) || strings.HasSuffix(deny, "."+h) {
				return deny, true
			}
		}
	}
	return "", false
}

// normHost lowercases and strips a scheme, path, port and trailing dot, so `https://PayPal.com:443/x`
// cannot slip past `paypal.com`.
func normHost(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil {
			s = u.Host
		}
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if u, err := url.Parse("//" + s); err == nil && u.Hostname() != "" {
		s = u.Hostname()
	}
	return strings.TrimSuffix(s, ".")
}

// checkClass3 is the mint, renew and queued-grant check for an S2 profile.
func (d *Class3Deny) checkClass3(hosts []string) error {
	if d == nil || len(hosts) == 0 {
		return ErrClass3Unchecked
	}
	if deny, hit := d.Match(hosts); hit {
		return fmt.Errorf("%w (%s)", ErrClass3Denied, deny)
	}
	return nil
}
