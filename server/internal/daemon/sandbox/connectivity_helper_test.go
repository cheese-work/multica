package sandbox

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Helper modes of the test binary. The sandbox mounts the binary at
// bridgeMount, so a fake agent is just that path plus one of these arguments.
const (
	probeArg   = "__probe"    // the connectivity client (CHE-2216 slice A7c)
	fakeMCPArg = "__fake-mcp" // a stdio MCP server
)

// helperMain runs a helper mode and reports whether os.Args asked for one.
func helperMain() (code int, ok bool) {
	if len(os.Args) < 2 {
		return 0, false
	}
	switch os.Args[1] {
	case BridgeHelperArg:
		return BridgeMain(os.Args[2:]), true
	case probeArg:
		if err := probe(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "probe failed:", err)
			return 1, true
		}
		return 0, true
	case fakeMCPArg:
		fakeMCP(os.Stdin, os.Stdout)
		return 0, true
	}
	return 0, false
}

// probe is the in-sandbox client. It honours HTTPS_PROXY the way a CLI does
// and trusts SSL_CERT_FILE; it prints "ok <kind> ..." on success.
func probe(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: kind target...")
	}
	kind, target := args[0], args[1]
	switch kind {
	case "http":
		return probeHTTP(target, "Bearer run-token", "pong")
	case "sse":
		return probeSSE(target)
	case "ws":
		return probeWS(target)
	case "mcp-http":
		return probeMCPHTTP(target)
	case "mcp-stdio":
		return probeMCPStdio(target, args[2:])
	case "direct": // no proxy at all: must have no route
		c, err := net.DialTimeout("tcp", target, 2*time.Second)
		if err != nil {
			fmt.Println("refused", target)
			return nil
		}
		c.Close()
		return fmt.Errorf("REACHED %s without the proxy", target)
	case "noproxy-http": // a client that ignores the proxy variables
		tr := &http.Transport{Proxy: nil}
		resp, err := (&http.Client{Transport: tr, Timeout: 2 * time.Second}).Get(target)
		if err == nil {
			resp.Body.Close()
			return fmt.Errorf("REACHED %s without the proxy", target)
		}
		fmt.Println("refused", target)
		return nil
	}
	return fmt.Errorf("unknown probe kind %q", kind)
}

func proxyTransport() (*http.Transport, error) {
	raw := os.Getenv("HTTPS_PROXY")
	u, err := url.Parse(raw)
	if err != nil || raw == "" {
		return nil, fmt.Errorf("HTTPS_PROXY %q unusable", raw)
	}
	// Explicit, not ProxyFromEnvironment: Go never proxies loopback targets.
	return &http.Transport{Proxy: http.ProxyURL(u)}, nil
}

func proxyClient() (*http.Client, error) {
	tr, err := proxyTransport()
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}, nil
}

func probeHTTP(target, auth, want string) error {
	c, err := proxyClient()
	if err != nil {
		return err
	}
	req, _ := http.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", auth)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
		return fmt.Errorf("status %d body %q", resp.StatusCode, body)
	}
	fmt.Println("ok http", resp.StatusCode)
	return nil
}

// probeSSE reads a streamed reply event by event; a tunnel that buffers the
// whole body would still pass, so it also checks the first event arrives
// before the server's last one is sent.
func probeSSE(target string) error {
	c, err := proxyClient()
	if err != nil {
		return err
	}
	c.Timeout = 0
	resp, err := c.Post(target, "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	events := 0
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			events++
		}
	}
	if events != 3 {
		return fmt.Errorf("got %d stream events, want 3", events)
	}
	fmt.Println("ok sse", events)
	return nil
}

func probeWS(target string) error {
	tr, err := proxyTransport()
	if err != nil {
		return err
	}
	d := websocket.Dialer{Proxy: tr.Proxy, HandshakeTimeout: 10 * time.Second}
	conn, resp, err := d.Dial(target, http.Header{"Authorization": {"Bearer run-token"}})
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = resp
	if err := conn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return err
	}
	if string(msg) != "echo:hello" {
		return fmt.Errorf("ws reply %q", msg)
	}
	fmt.Println("ok ws", string(msg))
	return nil
}

const mcpInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"gate","version":"0"}}}`

func probeMCPHTTP(target string) error {
	c, err := proxyClient()
	if err != nil {
		return err
	}
	resp, err := c.Post(target, "application/json", strings.NewReader(mcpInitialize))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkInitialize(resp.Body, "http")
}

func probeMCPStdio(command string, args []string) error {
	cmd := exec.Command(command, args...)
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	fmt.Fprintln(in, mcpInitialize)
	return checkInitialize(out, "stdio")
}

func checkInitialize(r io.Reader, transport string) error {
	var reply struct {
		Result struct {
			ServerInfo struct{ Name string } `json:"serverInfo"`
		} `json:"result"`
	}
	line, err := bufio.NewReader(r).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return err
	}
	if err := json.Unmarshal(line, &reply); err != nil || reply.Result.ServerInfo.Name == "" {
		return fmt.Errorf("no initialize result in %q", line)
	}
	fmt.Println("ok mcp-"+transport, reply.Result.ServerInfo.Name)
	return nil
}

// fakeMCP answers the initialize request on in with a server name on out.
func fakeMCP(in io.Reader, out io.Writer) {
	line, _ := bufio.NewReader(in).ReadBytes('\n')
	if len(line) == 0 {
		return
	}
	fmt.Fprintln(out, `{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"fake-stdio-mcp"}}}`)
}
