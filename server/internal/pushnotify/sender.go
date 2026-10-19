package pushnotify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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

// refusePrivate runs on the resolved address at connect time, so a DNS name
// that points at the internal network (or rebinds there) cannot be reached.
func refusePrivate(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("refusing to push to non-public address %s", host)
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
