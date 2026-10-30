package readproxy

import (
	"context"
	"fmt"
	"io"
)

// ShimHelperArg is os.Args[1] of the in-sandbox `gh` shim process.
const ShimHelperArg = "__gh-shim"

// ShimMain is the elevated run's `gh`. It speaks only to the proxy socket and
// only `read <node-id>`; anything else is refused, so a real GitHub call never
// starts from here. Exit 2 is a usage refusal, 1 a failed read.
func ShimMain(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) != 2 || args[0] != "read" {
		fmt.Fprintln(stderr, "gh: elevated runs support only `gh read <node-id>`")
		return 2
	}
	sock := getenv(SocketEnv)
	if sock == "" {
		fmt.Fprintln(stderr, "gh:", SocketEnv, "is not set")
		return 1
	}
	res, err := Read(context.Background(), sock, args[1])
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
