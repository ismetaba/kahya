// kahya-trigger is the tiny binary launchd execs for every declared job
// (W4-01 task spec step 3): it POSTs /jobs/trigger/{name} to kahyad over
// the UDS control socket (a 10s timeout bounds the whole call, dial
// included), prints kahyad's JSON response verbatim to stdout, and exits
// 0 on any 2xx response (the endpoint answers 202 - kahyad accepted the
// trigger and does ALL the actual work asynchronously, itself) or
// non-zero on anything else: a dial failure, a timeout, or a non-2xx
// status (a 404 unknown-job body is printed and this still exits
// non-zero).
//
// No other logic lives here on purpose: a manual `kahya-trigger <name>`
// invocation and a launchd StartCalendarInterval-scheduled run share this
// EXACT SAME code path (kahyad's own /jobs/trigger/{name} handler does
// every real decision - job lookup, ledgering, running the handler), so
// job-dispatch behavior can never diverge between "scheduled" and
// "manual" - there is exactly one way a job ever runs.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"kahya/kahyad/internal/config"
)

// requestTimeout bounds the ENTIRE call, dial included (task spec step
// 3: "POST /jobs/trigger/{name} with a 10s timeout").
const requestTimeout = 10 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main's testable core.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(stderr, "usage: kahya-trigger <job-name>")
		return 2
	}
	name := args[0]

	sock, err := resolveSocket()
	if err != nil {
		fmt.Fprintln(stderr, "kahya-trigger: resolve socket:", err)
		return 1
	}

	client := &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}

	endpoint := "http://kahyad/jobs/trigger/" + url.PathEscape(name)
	req, err := http.NewRequest(http.MethodPost, endpoint, nil)
	if err != nil {
		fmt.Fprintln(stderr, "kahya-trigger: build request:", err)
		return 1
	}

	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(stderr, "kahya-trigger: kahyad unreachable:", err)
		return 1
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintln(stderr, "kahya-trigger: read response:", err)
		return 1
	}
	fmt.Fprintln(stdout, string(body))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 1
	}
	return 0
}

// resolveSocket returns the UDS path to dial: KAHYA_SOCKET if set, else
// kahyad's own resolved default - mirroring kahya-mcp's/the kahya CLI's
// own resolveSocket exactly (kahyad/cmd/kahya-mcp/main.go,
// kahyad/cmd/kahya/client.go), so every Kahya UDS client agrees on the
// same socket path with no second copy of config.Load's defaulting/
// override rules.
func resolveSocket() (string, error) {
	if v := os.Getenv("KAHYA_SOCKET"); v != "" {
		return config.ExpandHome(v), nil
	}
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	return config.ExpandHome(cfg.Socket), nil
}
