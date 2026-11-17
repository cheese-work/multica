package daemon

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// runAPIMux is the daemon API a sandboxed run may reach: /health and
// /repo/checkout only (CHE-6503). Every other daemon route, including
// /shutdown and /maintenance/*, answers 404.
func (d *Daemon) runAPIMux(startedAt time.Time) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", d.healthHandler(startedAt))
	mux.HandleFunc("/repo/checkout", d.repoCheckoutHandler())
	return mux
}

// startRunAPI serves runAPIMux on a Unix socket created inside dir (mode 0600)
// and returns its path and a stop func that closes it and removes the socket.
// The caller binds dir, and only dir, into one run's sandbox.
func (d *Daemon) startRunAPI(dir string) (sock string, stop func(), err error) {
	sock = filepath.Join(dir, "daemon.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return "", nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return "", nil, err
	}
	srv := &http.Server{Handler: d.runAPIMux(time.Now())}
	go srv.Serve(ln)
	return sock, func() { srv.Close() }, nil // Close closes ln, which unlinks the socket
}
