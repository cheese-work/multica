// Package pushnotify delivers inbox records as Web Push notifications
// (CHE-1795). Rows reach the outbox through the inbox_item trigger (migration
// 846); the Worker here sends them once they are due and still owed.
package pushnotify

import "os"

// Config is the deployment's VAPID identity. Push is off unless all three
// values are set.
type Config struct {
	PublicKey  string
	PrivateKey string
	Subject    string
}

func ConfigFromEnv() Config {
	return Config{
		PublicKey:  os.Getenv("MULTICA_VAPID_PUBLIC_KEY"),
		PrivateKey: os.Getenv("MULTICA_VAPID_PRIVATE_KEY"),
		Subject:    os.Getenv("MULTICA_VAPID_SUBJECT"),
	}
}

func (c Config) Enabled() bool {
	return c.PublicKey != "" && c.PrivateKey != "" && c.Subject != ""
}
