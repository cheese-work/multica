package pushnotify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Subscription is a browser's push endpoint with its RFC 8291 keys.
type Subscription struct {
	Endpoint string
	P256dh   string
	Auth     string
}

// Sender delivers one encrypted payload and reports the push service's HTTP
// status. A non-nil error means no status was received.
type Sender interface {
	Send(ctx context.Context, sub Subscription, payload []byte) (int, error)
}

const (
	maxEndpointLen = 2048
	// A push is only worth showing while it is fresh.
	pushTTLSeconds = 3600
)

// ValidEndpoint accepts what a browser push service looks like: an https URL
// with a DNS name. Registration is user-controlled input that the server later
// POSTs to, so this is the first of two SSRF guards (the dialer is the second).
func ValidEndpoint(raw string) error {
	if len(raw) > maxEndpointLen {
		return errors.New("endpoint too long")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return errors.New("endpoint must be an https URL")
	}
	host := u.Hostname()
	if host == "" || net.ParseIP(host) != nil || !strings.Contains(host, ".") || strings.EqualFold(host, "localhost") {
		return errors.New("endpoint must name a public host")
	}
	return nil
}

// nonPublic lists the address blocks a push service is never served from: the
// IANA special-purpose registries for IPv4 and IPv6, plus the transition
// ranges that embed an IPv4 address (NAT64, 6to4, Teredo). The registries
// mark most of these "not globally reachable", but several (shared address
// space 100.64.0.0/10, benchmarking 198.18.0.0/15) are routed to internal
// services on real networks, which is why net.IP.IsPrivate is not enough.
var nonPublic = func() []netip.Prefix {
	blocks := []string{
		// IPv4
		"0.0.0.0/8",       // "this network"
		"10.0.0.0/8",      // private
		"100.64.0.0/10",   // shared address space (carrier-grade NAT)
		"127.0.0.0/8",     // loopback
		"169.254.0.0/16",  // link local, cloud metadata
		"172.16.0.0/12",   // private
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"192.88.99.0/24",  // 6to4 relay anycast
		"192.168.0.0/16",  // private
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved, includes the broadcast address
		// IPv6
		"::/96",          // unspecified, loopback, IPv4-compatible
		"64:ff9b::/96",   // NAT64
		"64:ff9b:1::/48", // local-use NAT64
		"100::/64",       // discard-only
		"2001::/23",      // IETF protocol assignments, includes Teredo
		"2001:db8::/32",  // documentation
		"2002::/16",      // 6to4
		"3fff::/20",      // documentation
		"5f00::/16",      // segment routing SIDs
		"fc00::/7",       // unique local
		"fe80::/10",      // link local
		"fec0::/10",      // deprecated site local
		"ff00::/8",       // multicast
	}
	prefixes := make([]netip.Prefix, len(blocks))
	for i, block := range blocks {
		prefixes[i] = netip.MustParsePrefix(block)
	}
	return prefixes
}()

// refusePrivate runs on the resolved address at connect time, so a DNS name
// that points at the internal network (or rebinds there) cannot be reached.
// An IPv4 address carried in IPv6 (::ffff:a.b.c.d) is judged as IPv4.
func refusePrivate(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("refusing to push to unreadable address %q", host)
	}
	ip = ip.WithZone("").Unmap()
	for _, block := range nonPublic {
		if block.Contains(ip) {
			return fmt.Errorf("refusing to push to non-public address %s", ip)
		}
	}
	return nil
}

// NewHTTPClient is the client production pushes go out on.
func NewHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: refusePrivate}
	return &http.Client{
		Timeout:       sendTimeout,
		Transport:     &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second, MaxIdleConnsPerHost: 4},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type webPushSender struct {
	cfg    Config
	client *http.Client
}

// NewWebPushSender sends over Web Push (VAPID + aes128gcm). Tests pass their
// own client; production uses NewHTTPClient.
func NewWebPushSender(cfg Config, client *http.Client) Sender {
	return &webPushSender{cfg: cfg, client: client}
}

func (s *webPushSender) Send(ctx context.Context, sub Subscription, payload []byte) (int, error) {
	resp, err := webpush.SendNotificationWithContext(ctx, payload,
		&webpush.Subscription{Endpoint: sub.Endpoint, Keys: webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth}},
		&webpush.Options{
			HTTPClient: s.client, Subscriber: s.cfg.Subject, VAPIDPublicKey: s.cfg.PublicKey, VAPIDPrivateKey: s.cfg.PrivateKey,
			TTL: pushTTLSeconds,
		})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}
