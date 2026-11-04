package pushnotify

import (
	"strings"
	"testing"
)

// CHE-1795: the connect-time guard let shared address space such as
// 100.64.0.0/10 through. The table checks the resolved-address policy alone: no
// connection is attempted and no host is resolved.

func TestConnectGuardRefusesNonPublicDestinations(t *testing.T) {
	refused := []string{
		// IPv4 special-use blocks (IANA registry), first and last address where the edge matters.
		"0.0.0.0", "0.1.2.3", "10.1.2.3", "100.64.0.1", "100.127.255.255", "127.0.0.1", "169.254.169.254",
		"172.16.0.1", "172.31.255.255", "192.0.0.8", "192.0.2.1", "192.88.99.1", "192.168.1.1",
		"198.18.0.1", "198.19.255.255", "198.51.100.1", "203.0.113.1", "224.0.0.1", "239.255.255.255",
		"240.0.0.1", "255.255.255.255",
		// IPv6 special-use blocks.
		"::", "::1", "::2", "64:ff9b::a00:1", "64:ff9b:1::1", "100::1", "2001::1", "2001:1::1", "2001:db8::1",
		"2002:a00:1::1", "5f00::1", "3fff::1", "fc00::1", "fd12:3456::1", "fe80::1", "fe80::1%eth0", "fec0::1", "ff02::1",
		// IPv4 carried inside IPv6 must follow the IPv4 policy.
		"::ffff:100.64.0.1", "::ffff:10.0.0.1", "::ffff:127.0.0.1", "::ffff:169.254.169.254",
	}
	for _, host := range refused {
		for _, address := range []string{hostPort(host)} {
			if err := refusePrivate("tcp", address, nil); err == nil {
				t.Errorf("%s was allowed, want it refused", address)
			} else if !strings.Contains(err.Error(), "non-public") {
				t.Errorf("%s refused with %q, want the non-public reason", address, err)
			}
		}
	}
}

func TestConnectGuardAllowsPublicDestinations(t *testing.T) {
	allowed := []string{
		"8.8.8.8", "17.253.144.10", "142.250.80.46", "44.0.0.1",
		// Neighbours of the blocked ranges stay reachable.
		"100.63.255.255", "100.128.0.1", "172.15.255.255", "172.32.0.1", "198.17.255.255", "198.20.0.1", "223.255.255.255",
		"2606:4700:4700::1111", "2a00:1450:4001:80b::200e", "2001:4860:4860::8888", "2001:200::1", "::ffff:8.8.8.8",
	}
	for _, host := range allowed {
		if err := refusePrivate("tcp", hostPort(host), nil); err != nil {
			t.Errorf("%s was refused: %v", host, err)
		}
	}
}

func TestConnectGuardRefusesWhatItCannotRead(t *testing.T) {
	for _, address := range []string{"push.example.com:443", "8.8.8.8", "[::1", ""} {
		if err := refusePrivate("tcp", address, nil); err == nil {
			t.Errorf("%q was allowed", address)
		}
	}
}

func hostPort(host string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:443"
	}
	return host + ":443"
}
