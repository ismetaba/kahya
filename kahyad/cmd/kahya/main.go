// kahya is the Kâhya CLI (W12-06): the primary interaction surface for
// W1-5 (HANDOFF §4 UI). No args starts a REPL; a first argument that isn't
// a known subcommand is treated as a one-shot question; "log", "health",
// and "reindex" are subcommands. Everything talks to kahyad over the UDS
// control socket (client.go); every user-facing string lives in
// strings.go, in Turkish, per HANDOFF §3's language policy.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"kahya/kahyad/internal/traceid"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is main's testable core: it takes argv (without argv[0]) and the
// three standard streams explicitly, so tests can drive it against
// in-memory buffers and a fake UDS server instead of the real process
// streams/socket.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	sock, err := resolveSocket()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	client := newClient(sock)

	if len(args) == 0 {
		return runREPL(client, stdin, stdout, stderr)
	}

	switch args[0] {
	case "log":
		return runLog(client, args[1:], stdout, stderr)
	case "health":
		return runHealth(client, stdout, stderr)
	case "reindex":
		return runReindex(client, args[1:], stdout, stderr)
	default:
		return runOneShot(client, args, stdout, stderr)
	}
}

// runOneShot handles `kahya <question...>` (W12-06 step 2). The prompt is
// argv joined with spaces; an empty/whitespace-only prompt is rejected
// LOCALLY, before any dial, per the task spec's test list.
func runOneShot(client *Client, args []string, stdout, stderr io.Writer) int {
	prompt := strings.TrimSpace(strings.Join(args, " "))
	if prompt == "" {
		fmt.Fprintln(stderr, MsgEmptyQuestion)
		return 2
	}
	traceID := traceid.New()
	return execTask(client, traceID, prompt, stdout, stderr)
}

// execTask runs one task to completion: POST /v1/task, stream delta text
// to stdout, print the trace footer to stderr, and return the exit code
// (W12-06 step 2: 0 on result.status=="ok", 1 on error, 2 on any transport
// failure - dial failure or, until W12-07 lands, /v1/task's current 404).
func execTask(client *Client, traceID, prompt string, stdout, stderr io.Writer) int {
	res, err := client.StreamTask(context.Background(), traceID, prompt, func(text string) {
		fmt.Fprint(stdout, text)
	})
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		printTraceFooter(stderr, traceID)
		return 2
	}
	printTraceFooter(stderr, traceID)
	if res.Status == "ok" {
		return 0
	}
	if res.ErrMsg != "" {
		fmt.Fprintln(stderr, res.ErrMsg)
	}
	return 1
}

// runREPL implements the REPL (W12-06 step 3): a banner, then a
// read-eval-print loop over bufio.Scanner (no readline dependency); "/çık"
// or "/cik", or EOF/Ctrl-D, ends it. Each non-empty line runs one task with
// a fresh trace_id. The REPL itself always exits 0 - a failed task inside
// the loop is reported (via execTask's own stderr output) but does not end
// the session or change its exit code.
func runREPL(client *Client, stdin io.Reader, stdout, stderr io.Writer) int {
	fmt.Fprintln(stdout, MsgREPLBanner)
	sc := bufio.NewScanner(stdin)
	for {
		fmt.Fprint(stdout, MsgREPLPrompt)
		if !sc.Scan() {
			break // EOF/Ctrl-D
		}
		line := strings.TrimSpace(sc.Text())
		if line == "/çık" || line == "/cik" {
			break
		}
		if line == "" {
			continue
		}
		execTask(client, traceid.New(), line, stdout, stderr)
	}
	fmt.Fprintln(stdout, MsgFarewell)
	return 0
}

// runHealth implements `kahya health` (W12-06 step 5): GET /health, print
// the Turkish health line, nonzero exit if unreachable or degraded.
func runHealth(client *Client, stdout, stderr io.Writer) int {
	hr, err := client.Health(context.Background(), traceid.New())
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	fmt.Fprintf(stdout, MsgHealthOK+"\n", hr.PID, hr.SchemaVersion)
	if hr.DB != "ok" {
		fmt.Fprintf(stderr, MsgHealthDegraded+"\n", hr.DB)
		return 1
	}
	return 0
}

// runReindex implements `kahya reindex [--full]` (W12-06 step 6): POST
// /v1/reindex, print the Turkish summary from the response.
func runReindex(client *Client, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reindex", flag.ContinueOnError)
	fs.SetOutput(stderr)
	full := fs.Bool("full", false, "tam yeniden indeksleme")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	rr, err := client.Reindex(context.Background(), traceid.New(), *full)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	fmt.Fprintf(stdout, MsgReindexSummary+"\n", rr.FilesIndexed, rr.Chunks, rr.DurationMs)
	return 0
}

// runLog implements `kahya log --trace <id> [--raw]` (W12-06 step 4): GET
// /v1/log, then either dump raw JSONL (--raw) or pretty-print each line as
// "HH:MM:SS.mmm  LEVEL  [proc]  event  key=val…".
func runLog(client *Client, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	fs.SetOutput(stderr)
	trace := fs.String("trace", "", "render this trace_id's log lines")
	raw := fs.Bool("raw", false, "dump raw JSONL instead of pretty-printing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	*trace = strings.TrimSpace(*trace)
	if *trace == "" {
		fmt.Fprintln(stderr, MsgTraceRequired)
		return 2
	}

	lines, err := client.Log(context.Background(), traceid.New(), *trace)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	if len(lines) == 0 {
		fmt.Fprintf(stderr, MsgLogNotFound+"\n", *trace)
		return 1
	}

	if *raw {
		for _, l := range lines {
			b, err := json.Marshal(l)
			if err != nil {
				continue
			}
			fmt.Fprintln(stdout, string(b))
		}
		return 0
	}
	for _, l := range lines {
		fmt.Fprintln(stdout, formatLogLine(l))
	}
	return 0
}

// formatLogLine renders one decoded JSONL line as
// "HH:MM:SS.mmm  LEVEL  [proc]  event  key=val…" (W12-06 step 4). ts is
// reformatted from RFC3339Nano to a millisecond-precision wall clock time;
// proc/level/event/trace_id are pulled out as the fixed columns (trace_id
// is omitted from the trailing key=val… list since every line here already
// shares the one trace_id the caller asked for); every other key is
// appended sorted by key, for deterministic output.
func formatLogLine(m map[string]any) string {
	ts := ""
	if s, ok := m["ts"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			ts = t.Format("15:04:05.000")
		} else {
			ts = s
		}
	}
	level, _ := m["level"].(string)
	proc, _ := m["proc"].(string)
	event, _ := m["event"].(string)

	keys := make([]string, 0, len(m))
	for k := range m {
		switch k {
		case "ts", "level", "proc", "event", "trace_id":
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	line := fmt.Sprintf("%s  %s  [%s]  %s", ts, level, proc, event)
	for _, k := range keys {
		line += fmt.Sprintf("  %s=%v", k, m[k])
	}
	return line
}

// printTraceFooter prints MsgTraceFooter (trace_id substituted), dimmed
// with an ANSI SGR "faint" sequence when w is a real TTY (W12-06 step 1:
// "trace footer after each answer: `iz: %s` (dim/faint if TTY)"). In tests
// w is an in-memory buffer, never *os.File, so this is always the plain,
// uncolored branch there - exactly what byte-exact string assertions need.
func printTraceFooter(w io.Writer, traceID string) {
	msg := fmt.Sprintf(MsgTraceFooter, traceID)
	if f, ok := w.(*os.File); ok && isTerminal(f) {
		fmt.Fprintln(w, "\x1b[2m"+msg+"\x1b[0m")
		return
	}
	fmt.Fprintln(w, msg)
}

// isTerminal reports whether f looks like a character-device (TTY) rather
// than a pipe/regular file. This is an approximation (no ioctl-based
// isatty check, to avoid a syscall-per-platform dependency for a purely
// cosmetic faint/dim toggle) but is the standard trick for this purpose
// without pulling in golang.org/x/term.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
