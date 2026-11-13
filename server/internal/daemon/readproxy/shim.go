package readproxy

import (
	"context"
	"fmt"
	"io"
)

// ShimHelperArg is os.Args[1] of the in-sandbox `gh` shim process.
const ShimHelperArg = "__gh-shim"

// ShimMain is the elevated run's `gh`. It speaks only to the proxy socket and
// only reads; anything else is refused, so a real GitHub call never
// starts from here. Exit 2 is a usage refusal, 1 a failed read.
func ShimMain(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	var call func(sock string) (Result, error)
	switch {
	case len(args) == 2 && args[0] == "read":
		call = func(sock string) (Result, error) { return Read(context.Background(), sock, args[1]) }
	case len(args) == 3 && args[0] == "read":
		call = func(sock string) (Result, error) { return ReadPR(context.Background(), sock, args[1], args[2]) }
	case len(args) == 3 && args[0] == "log":
		call = func(sock string) (Result, error) { return ReadCILog(context.Background(), sock, args[1], args[2]) }
	case len(args) == 3 && args[0] == "commit":
		call = func(sock string) (Result, error) { return ReadCommit(context.Background(), sock, args[1], args[2]) }
	default:
		fmt.Fprintln(stderr, "gh: elevated runs support only `gh read <node-id> [title|body]`, `gh log <owner/repo> <job-id>` and `gh commit <owner/repo> <sha>`")
		return 2
	}
	sock := getenv(SocketEnv)
	if sock == "" {
		fmt.Fprintln(stderr, "gh:", SocketEnv, "is not set")
		return 1
	}
	res, err := call(sock)
	if err != nil {
		fmt.Fprintln(stderr, "gh:", err)
		return 1
	}
	if res.Full {
		if _, err := io.WriteString(stdout, res.Body); err != nil {
			fmt.Fprintln(stderr, "gh:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "[withheld] %s %s\nurl: %s\nsha256: %s\n", res.Kind, res.ID, res.URL, res.SHA256)
	return 0
}
