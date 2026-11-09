// Package gateprobe is the in-sandbox client of the CHE-2216 slice A7c
// connectivity gate. Test binaries re-execute themselves inside the sandbox
// with ProbeArg or FakeMCPArg and call Main from TestMain, so the "agent" that
// connects out is real code running behind the real bridge. Test support only.
package gateprobe

import (
	"bufio"
	"context"
	"crypto/tls"
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

const (
	ProbeArg   = "__probe"    // connectivity client: kind target
	FakeMCPArg = "__fake-mcp" // stdio MCP server that needs GateKeyEnv

	// GateKeyEnv is the variable a stdio MCP entry must pass to its server; the
	// fake answers with its value, so a dropped `env` field fails the gate.
	GateKeyEnv = "GATE_MCP_KEY"
	// SSEAckPath is requested once per stream, after its first event.
	SSEAckPath = "/v1/stream/ack"
)

// MCPEntry is the part of a runtime MCP server entry the gate exercises.
type MCPEntry struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// Main runs a helper mode when args (os.Args[1:]) ask for one.
func Main(args []string) (code int, ok bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case ProbeArg:
		if err := probe(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "probe failed:", err)
			return 1, true
		}
		return 0, true
	case FakeMCPArg:
		FakeMCP(os.Stdin, os.Stdout)
		return 0, true
	}
	return 0, false
}

// probe honours HTTPS_PROXY the way a CLI does and trusts SSL_CERT_FILE; it
// prints "ok <kind> ..." on success.
func probe(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: kind target")
	}
	kind, target := args[0], args[1]
	switch kind {
	case "http":
		return probeHTTP(target)
	case "sse":
		return probeSSE(target)
	case "ws":
		return probeWS(target)
	case "mcp":
		return probeMCP(target)
	case "direct": // no proxy at all: must have no route
		c, err := net.DialTimeout("tcp", target, 2*time.Second)
		if err != nil {
			fmt.Println("refused", target)
			return nil
		}
		c.Close()
		return fmt.Errorf("REACHED %s without the proxy", target)
	case "connect": // raw CONNECT to the proxy: connect <authority> [sni]
		return probeConnect(target, args[2:])
	case "absolute": // absolute-form request to the proxy instead of CONNECT
		return probeAbsolute(target)
	case "dns": // a name lookup with no proxy: must get no answer
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if addrs, err := net.DefaultResolver.LookupHost(ctx, target); err == nil {
			return fmt.Errorf("RESOLVED %s to %v without the proxy", target, addrs)
		}
		fmt.Println("refused", target)
		return nil
	case "noproxy-http": // a client that ignores the proxy variables
		client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 2 * time.Second}
		resp, err := client.Get(target)
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

func probeHTTP(target string) error {
	c, err := proxyClient()
	if err != nil {
		return err
	}
	req, _ := http.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer run-token")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "pong") {
		return fmt.Errorf("status %d body %q", resp.StatusCode, body)
	}
	fmt.Println("ok http", resp.StatusCode)
	return nil
}

// probeSSE reads a stream event by event. The server holds back events 1 and 2
// until the client acknowledges event 0 on a second request, so a tunnel that
// buffers the body can never finish: the first event would never arrive.
func probeSSE(target string) error {
	c, err := proxyClient()
	if err != nil {
		return err
	}
	c.Timeout = 0
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(`{"stream":true}`))
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	events := 0
	for sc.Scan() {
		if !strings.HasPrefix(sc.Text(), "data: ") {
			continue
		}
		events++
		if events == 1 {
			u, err := url.Parse(target)
			if err != nil {
				return err
			}
			u.Path = SSEAckPath
			ack, _ := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
			r, err := c.Do(ack)
			if err != nil {
				return fmt.Errorf("ack after first event: %w", err)
			}
			r.Body.Close()
		}
	}
	if events != 3 {
		if err := sc.Err(); err != nil {
			return fmt.Errorf("got %d stream events, want 3: %w", events, err)
		}
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
	if resp != nil {
		resp.Body.Close() // the upgrade response body is empty; the socket lives on conn
	}
	if err != nil {
		return err
	}
	defer conn.Close()
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

// probeMCP starts or calls one MCP server the way an agent CLI would, from
// entry (a JSON MCPEntry): a command runs with its args and env, a url is
// called with its headers.
func probeMCP(raw string) error {
	var e MCPEntry
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		return fmt.Errorf("MCP entry: %w", err)
	}
	switch {
	case e.Command != "":
		return probeMCPStdio(e)
	case e.URL != "":
		return probeMCPHTTP(e)
	}
	return errors.New("MCP entry has neither command nor url")
}

func probeMCPHTTP(e MCPEntry) error {
	c, err := proxyClient()
	if err != nil {
		return err
	}
	req, _ := http.NewRequest(http.MethodPost, e.URL, strings.NewReader(mcpInitialize))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.Headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("MCP server answered status %d", resp.StatusCode)
	}
	return checkInitialize(resp.Body, "http")
}

func probeMCPStdio(e MCPEntry) error {
	cmd := exec.Command(e.Command, e.Args...)
	cmd.Env = os.Environ()
	for k, v := range e.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
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

// FakeMCP answers the initialize request on in. Its server name carries the
// value of GateKeyEnv; without that variable it answers with an error.
func FakeMCP(in io.Reader, out io.Writer) {
	line, _ := bufio.NewReader(in).ReadBytes('\n')
	if len(line) == 0 {
		return
	}
	key := os.Getenv(GateKeyEnv)
	if key == "" {
		fmt.Fprintln(out, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"no `+GateKeyEnv+` in the server environment"}}`)
		return
	}
	fmt.Fprintf(out, `{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"fake-stdio-mcp:%s"}}}`+"\n", key)
}

// proxyConn opens the TCP connection to the proxy named by HTTPS_PROXY.
func proxyConn() (net.Conn, error) {
	u, err := url.Parse(os.Getenv("HTTPS_PROXY"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("HTTPS_PROXY unusable")
	}
	return net.DialTimeout("tcp", u.Host, 5*time.Second)
}

// probeConnect sends CONNECT authority exactly as given, with no client-side
// normalization, and prints the status. Given an sni and a 200, it starts a
// TLS handshake with that server name and prints whether it completed (the
// handshake does not verify the peer: only the proxy's SNI check is under test).
func probeConnect(authority string, sni []string) error {
	c, err := proxyConn()
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority)
	br := bufio.NewReader(c)
	// Body stays unread and unclosed: after a 200 it is the live tunnel, and
	// closing it would block on the socket. The deferred c.Close() ends it.
	resp, err := http.ReadResponse(br, nil) //nolint:bodyclose // see above
	if err != nil {
		return fmt.Errorf("proxy answer: %w", err)
	}
	line := fmt.Sprintf("connect %s status=%d", authority, resp.StatusCode)
	if resp.StatusCode == http.StatusOK && len(sni) > 0 {
		t := tls.Client(&bufferedConn{Conn: c, r: br}, &tls.Config{ServerName: sni[0], InsecureSkipVerify: true})
		if err := t.Handshake(); err != nil {
			line += " tls=fail"
		} else {
			line += " tls=ok"
		}
	}
	fmt.Println(line)
	return nil
}

// bufferedConn reads through br so bytes read past the CONNECT reply are kept.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func probeAbsolute(target string) error {
	c, err := proxyConn()
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	u, err := url.Parse(target)
	if err != nil {
		return err
	}
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, u.Host)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil) //nolint:bodyclose // only the status line is used; the deferred c.Close() ends the socket
	if err != nil {
		return fmt.Errorf("proxy answer: %w", err)
	}
	fmt.Printf("absolute %s status=%d\n", target, resp.StatusCode)
	return nil
}
