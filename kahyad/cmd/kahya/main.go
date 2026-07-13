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
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"kahya/kahyad/internal/approval"
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
	case "autonomy":
		return runAutonomy(client, args[1:], stdout, stderr)
	case "undo":
		return runUndo(client, args[1:], stdout, stderr)
	case "approvals":
		return runApprovals(client, stdout, stderr)
	case "approve":
		return runApprove(client, args[1:], stdin, stdout, stderr)
	case "task":
		return runTask(client, args[1:], stdout, stderr)
	case "ledger":
		return runLedger(client, args[1:], stdout, stderr)
	case "ask":
		return runAsk(client, args[1:], stdout, stderr)
	case "job":
		return runJob(client, args[1:], stdout, stderr)
	case "consolidation":
		return runConsolidation(client, args[1:], stdin, stdout, stderr)
	case "fact":
		return runFact(client, args[1:], stdout, stderr)
	case "entity":
		return runEntity(client, args[1:], stdout, stderr)
	case "remembered":
		return runRemembered(client, args[1:], stdout, stderr)
	case "eval":
		return runEval(client, args[1:], stdout, stderr)
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
	return execTask(client, traceID, prompt, false, stdout, stderr)
}

// runAsk implements `kahya ask [--derin] <question...>` (W4-08): the SAME
// one-shot task execution as runOneShot, plus the --derin flag that pins
// claude-fable-5 ("derin düşün" opt-in) via envelope.deep_think. The OTHER
// opt-in form - the byte-exact Turkish prompt prefix "derin düşün:" - needs
// no flag at all; it is detected server-side regardless of which CLI
// subcommand sent the prompt.
func runAsk(client *Client, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.SetOutput(stderr)
	derin := fs.Bool("derin", false, "derin düşün (claude-fable-5 kullanır, ek maliyetlidir)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		fmt.Fprintln(stderr, MsgEmptyQuestion)
		return 2
	}
	traceID := traceid.New()
	return execTask(client, traceID, prompt, *derin, stdout, stderr)
}

// execTask runs one task to completion: POST /v1/task, stream delta text
// to stdout, print the trace footer to stderr, and return the exit code
// (W12-06 step 2: 0 on result.status=="ok", 1 on error, 2 on any transport
// failure - dial failure or, until W12-07 lands, /v1/task's current 404).
// deepThink is W4-08's --derin/deep_think opt-in.
func execTask(client *Client, traceID, prompt string, deepThink bool, stdout, stderr io.Writer) int {
	res, err := client.StreamTask(context.Background(), traceID, prompt, deepThink, func(text string) {
		fmt.Fprint(stdout, text)
	})
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		printTraceFooter(stderr, traceID)
		return 2
	}
	if res.Status == "ok" && res.ProcessedLocally {
		// W3-08 CLI badge: printed on its own line, after the streamed
		// answer text, before the trace footer.
		fmt.Fprintln(stdout, MsgLocallyProcessed)
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
// read-eval-print loop over a bufio.Reader (no readline dependency, and -
// unlike bufio.Scanner's default 64KB token cap, BLOCKER 2 - no line-length
// limit at all); "/çık" or "/cik", or EOF/Ctrl-D, ends it. Each non-empty
// line runs one task with a fresh trace_id. The REPL itself always exits 0
// - a failed task inside the loop is reported (via execTask's own stderr
// output) but does not end the session or change its exit code.
func runREPL(client *Client, stdin io.Reader, stdout, stderr io.Writer) int {
	fmt.Fprintln(stdout, MsgREPLBanner)
	r := bufio.NewReader(stdin)
	for {
		fmt.Fprint(stdout, MsgREPLPrompt)
		raw, err := r.ReadString('\n')
		if raw == "" && err != nil {
			break // EOF/Ctrl-D with nothing left to process
		}
		// Mirror bufio.ScanLines' framing (strip one trailing "\n", then one
		// trailing "\r") before the same TrimSpace the old Scanner-based loop
		// applied, so behavior is unchanged beyond removing the line cap.
		line := strings.TrimSuffix(raw, "\n")
		line = strings.TrimSuffix(line, "\r")
		line = strings.TrimSpace(line)
		if line == "/çık" || line == "/cik" {
			break
		}
		if line != "" {
			execTask(client, traceid.New(), line, false, stdout, stderr)
		}
		if err != nil {
			break // EOF right after a final, newline-less line
		}
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

// runReindex implements `kahya reindex [--full] [--re-embed]` (W12-06 step
// 6; W12-11 step 5 adds --re-embed): POST /v1/reindex, print the Turkish
// summary from the response.
func runReindex(client *Client, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reindex", flag.ContinueOnError)
	fs.SetOutput(stderr)
	full := fs.Bool("full", false, "tam yeniden indeksleme")
	reEmbed := fs.Bool("re-embed", false, "tum parçaları aktif embed modeline göre yeniden göm (model_ver değişiminde kullanılır)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	rr, err := client.Reindex(context.Background(), traceid.New(), *full, *reEmbed)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	fmt.Fprintf(stdout, MsgReindexSummary+"\n", rr.FilesIndexed, rr.Chunks, rr.DurationMs)
	return 0
}

// runJob implements `kahya job run <name>` (W5-01: extends the W4-01
// kahya-trigger mechanism with a subcommand on the main CLI). This POSTs
// to the EXACT SAME /jobs/trigger/{name} route kahya-trigger and a
// launchd-scheduled run both already use (kahyad/internal/server/jobs.go's
// own doc comment: "kahyad's ONE dispatch route") - a manual `kahya job
// run morning-briefing` can therefore never behave differently than the
// 08:30 scheduled run.
func runJob(client *Client, args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 || args[0] != "run" || strings.TrimSpace(args[1]) == "" {
		fmt.Fprintln(stderr, MsgJobUsage)
		return 2
	}
	name := args[1]

	traceID, err := client.TriggerJob(context.Background(), traceid.New(), name)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	fmt.Fprintf(stdout, MsgJobTriggered+"\n", name, traceID)
	return 0
}

// runConsolidation implements `kahya consolidation show|approve|reject`
// (W5-02): show renders the pending suggestion's diff (empty + Turkish
// notice when there is none); approve prints the diff again (WYSIWYE:
// the human sees exactly what will be merged before deciding) then gates
// on the literal "onayla" word (PromptConsolidationApprove), exactly like
// `kahya approve <id>`'s own W3 gate; reject is immediate, no diff
// re-render needed (nothing is being applied).
func runConsolidation(client *Client, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, MsgConsolidationUsage)
		return 2
	}
	switch args[0] {
	case "show":
		return runConsolidationShow(client, stdout, stderr)
	case "approve":
		return runConsolidationApprove(client, stdin, stdout, stderr)
	case "reject":
		return runConsolidationReject(client, stdout, stderr)
	default:
		fmt.Fprintln(stderr, MsgConsolidationUsage)
		return 2
	}
}

func runConsolidationShow(client *Client, stdout, stderr io.Writer) int {
	found, diff, err := client.ShowConsolidation(context.Background(), traceid.New())
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	if !found {
		fmt.Fprintln(stdout, MsgConsolidationEmpty)
		return 0
	}
	fmt.Fprint(stdout, diff)
	return 0
}

func runConsolidationApprove(client *Client, stdin io.Reader, stdout, stderr io.Writer) int {
	found, diff, err := client.ShowConsolidation(context.Background(), traceid.New())
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	if !found {
		fmt.Fprintln(stdout, MsgConsolidationEmpty)
		return 0
	}
	fmt.Fprint(stdout, diff)

	r := bufio.NewReader(stdin)
	decision, err := approval.PromptLiteral(r, stdout, PromptConsolidationApprove, "onayla")
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	if decision != approval.DecisionApprove {
		fmt.Fprintln(stdout, MsgApprovalDenied)
		return 1
	}

	if err := client.ApproveConsolidation(context.Background(), traceid.New()); err != nil {
		if errors.Is(err, errConsolidationNoPending) {
			fmt.Fprintln(stdout, MsgConsolidationEmpty)
			return 0
		}
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	fmt.Fprintln(stdout, MsgConsolidationApproved)
	return 0
}

func runConsolidationReject(client *Client, stdout, stderr io.Writer) int {
	if err := client.RejectConsolidation(context.Background(), traceid.New()); err != nil {
		if errors.Is(err, errConsolidationNoPending) {
			fmt.Fprintln(stdout, MsgConsolidationEmpty)
			return 0
		}
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	fmt.Fprintln(stdout, MsgConsolidationRejected)
	return 0
}

// runFact implements `kahya fact confirm <id>` and `kahya fact retract
// <özne> <yüklem> <nesne> [oturum_id]` (W5-04).
func runFact(client *Client, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, MsgFactUsage)
		return 2
	}
	switch args[0] {
	case "confirm":
		return runFactConfirm(client, args[1:], stdout, stderr)
	case "retract":
		return runFactRetract(client, args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, MsgFactUsage)
		return 2
	}
}

// runFactConfirm implements `kahya fact confirm <id>`: lifts an
// agent_derived fact's quarantine (kahyad/internal/factengine.Engine.
// ConfirmFact) - the human confirmation half of HANDOFF S5 memory #1.
func runFactConfirm(client *Client, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, MsgFactUsage)
		return 2
	}
	id, err := strconv.ParseInt(strings.TrimSpace(args[0]), 10, 64)
	if err != nil || id == 0 {
		fmt.Fprintln(stderr, MsgFactUsage)
		return 2
	}
	if err := client.ConfirmFact(context.Background(), traceid.New(), id); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	fmt.Fprintf(stdout, MsgFactConfirmed+"\n", id)
	return 0
}

// runFactRetract implements `kahya fact retract <özne> <yüklem> <nesne>
// [oturum_id]`: closes the ACTIVE fact matching that triple
// (status=retracted, valid_to set, negative evidence row - never a
// delete, HANDOFF S5 memory #3).
func runFactRetract(client *Client, args []string, stdout, stderr io.Writer) int {
	if len(args) < 3 || len(args) > 4 {
		fmt.Fprintln(stderr, MsgFactUsage)
		return 2
	}
	subject, predicate, object := args[0], args[1], args[2]
	sessionID := ""
	if len(args) == 4 {
		sessionID = args[3]
	}
	id, err := client.RetractFact(context.Background(), traceid.New(), subject, predicate, object, sessionID)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	fmt.Fprintf(stdout, MsgFactRetracted+"\n", id)
	return 0
}

// runEntity implements `kahya entity merge <a> <b> --evidence <fact_id>`
// and `kahya entity split <merge_ledger_id>` (W5-04).
func runEntity(client *Client, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, MsgEntityUsage)
		return 2
	}
	switch args[0] {
	case "merge":
		return runEntityMerge(client, args[1:], stdout, stderr)
	case "split":
		return runEntitySplit(client, args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, MsgEntityUsage)
		return 2
	}
}

// runEntityMerge implements `kahya entity merge <a> <b> --evidence
// <fact_id>`: b (src) merges INTO a (dst, survives) - --evidence MUST
// name a real, existing fact (HANDOFF S5 memory #2: name similarity
// alone never suffices). The two entity-id positionals come BEFORE the
// flag (mirroring runTaskResolve's identical "positional id, then
// flag.Parse the rest" shape), since flag.Parse would otherwise stop at
// the first non-flag argument.
func runEntityMerge(client *Client, args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, MsgEntityMergeUsage)
		return 2
	}
	aStr, bStr := args[0], args[1]

	fs := flag.NewFlagSet("merge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	evidence := fs.Int64("evidence", 0, "ayirt edici kanit olgu id'si (zorunlu)")
	if err := fs.Parse(args[2:]); err != nil {
		return 2
	}

	dstID, err1 := strconv.ParseInt(strings.TrimSpace(aStr), 10, 64)
	srcID, err2 := strconv.ParseInt(strings.TrimSpace(bStr), 10, 64)
	if err1 != nil || err2 != nil || *evidence == 0 {
		fmt.Fprintln(stderr, MsgEntityMergeUsage)
		return 2
	}

	id, err := client.MergeEntities(context.Background(), traceid.New(), dstID, srcID, *evidence, "user")
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	fmt.Fprintf(stdout, MsgEntityMerged+"\n", id)
	return 0
}

// runEntitySplit implements `kahya entity split <merge_ledger_id>`:
// restores both entities to their pre-merge state.
func runEntitySplit(client *Client, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, MsgEntitySplitUsage)
		return 2
	}
	mergeLedgerID, err := strconv.ParseInt(strings.TrimSpace(args[0]), 10, 64)
	if err != nil {
		fmt.Fprintln(stderr, MsgEntitySplitUsage)
		return 2
	}
	id, err := client.SplitEntities(context.Background(), traceid.New(), mergeLedgerID, "user")
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	fmt.Fprintf(stdout, MsgEntitySplit+"\n", id)
	return 0
}

// runRemembered implements `kahya remembered --trace <id>` (W5-03): the
// CLI half of the "hatırladı anı" marking flow (the Telegram "🌟
// Hatırladı" button is the other half, kahyad/internal/telegram). POSTs
// /v1/remembered with channel="local" - success (fresh mark OR an
// idempotent re-mark alike) prints the byte-exact Turkish success line;
// an unknown trace_id prints the server's own Turkish error message
// verbatim and exits nonzero.
func runRemembered(client *Client, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("remembered", flag.ContinueOnError)
	fs.SetOutput(stderr)
	trace := fs.String("trace", "", "hatırladı anı olarak işaretlenecek trace_id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	*trace = strings.TrimSpace(*trace)
	if *trace == "" {
		fmt.Fprintln(stderr, MsgRememberedTraceRequired)
		return 2
	}

	if _, err := client.MarkRemembered(context.Background(), traceid.New(), *trace); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	fmt.Fprintln(stdout, MsgRememberedSaved)
	return 0
}

// runEval implements `kahya eval` (W5-05) - currently just the one "mini"
// subcommand; any other/missing argument prints usage and exits 2.
func runEval(client *Client, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "mini" {
		fmt.Fprintln(stderr, MsgEvalUsage)
		return 2
	}
	return runEvalMini(client, stdout, stderr)
}

// runEvalMini implements `kahya eval mini`: POSTs /v1/eval/mini/run (kahyad
// runs the baseline against its own memory_search and ledgers the
// eval.mini.run event - this CLI process never opens brain.db itself),
// prints a GEÇTİ/KALDI line per question, a pass-count summary, and the
// regression verdict against the immediately preceding run. Exit code is
// NON-ZERO iff the server reports a regression (task spec: "regression = a
// previously-passing question now failing, or pass-count dropping") - a
// clean daemon-unreachable/server error also exits nonzero, same as every
// other subcommand.
func runEvalMini(client *Client, stdout, stderr io.Writer) int {
	result, err := client.EvalMiniRun(context.Background(), traceid.New())
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}

	for _, r := range result.Results {
		if r.Pass {
			fmt.Fprintf(stdout, MsgEvalMiniPass+"\n", r.Q)
		} else {
			fmt.Fprintf(stdout, MsgEvalMiniFail+"\n", r.Q)
		}
	}
	fmt.Fprintf(stdout, MsgEvalMiniSummary+"\n", result.PassCount, result.Total)

	if !result.PreviousFound {
		fmt.Fprintln(stdout, MsgEvalMiniFirstRun)
		return 0
	}
	if result.Regressed {
		fmt.Fprintf(stdout, MsgEvalMiniRegression+"\n", strings.Join(result.Reasons, "\n"))
		return 1
	}
	fmt.Fprintln(stdout, MsgEvalMiniNoRegression)
	return 0
}

// runAutonomy implements `kahya autonomy` (list ladder state) and
// `kahya autonomy promote <tool> <class> <scope>` (W3-02's ONLY
// promotion path - the user must invoke this by hand; nothing else in
// the system ever raises a ladder level on its own).
func runAutonomy(client *Client, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return runAutonomyList(client, stdout, stderr)
	}
	if args[0] == "promote" {
		return runAutonomyPromote(client, args[1:], stdout, stderr)
	}
	fmt.Fprintln(stderr, MsgAutonomyUsage)
	return 2
}

// runAutonomyList implements `kahya autonomy`: GET /policy/state, printed
// as one line per (tool, class, scope) triple.
func runAutonomyList(client *Client, stdout, stderr io.Writer) int {
	states, err := client.PolicyState(context.Background(), traceid.New())
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	if len(states) == 0 {
		fmt.Fprintln(stdout, MsgAutonomyEmpty)
		return 0
	}
	for _, st := range states {
		fmt.Fprintf(stdout, MsgAutonomyRow+"\n", st.Tool, st.Class, st.Scope, st.Level, st.ConsecutiveApprovals)
	}
	return 0
}

// runAutonomyPromote implements `kahya autonomy promote <tool> <class>
// <scope>`.
func runAutonomyPromote(client *Client, args []string, stdout, stderr io.Writer) int {
	if len(args) != 3 {
		fmt.Fprintln(stderr, MsgAutonomyPromoteUsage)
		return 2
	}
	tool, class, scope := args[0], args[1], args[2]
	level, err := client.PolicyPromote(context.Background(), traceid.New(), tool, class, scope)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	fmt.Fprintf(stdout, MsgAutonomyPromoted+"\n", tool, class, scope, level)
	return 0
}

// runUndo implements `kahya undo --trace <id>`: triggers the registered
// undo window for that trace while it is still open (HANDOFF S4 ladder
// L2 row - 5-minute grace period on an auto-allowed W1 write). Recipe
// EXECUTION itself is delegated to the owning tool (W3-03) - this command
// only opens/closes the window server-side.
func runUndo(client *Client, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("undo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	trace := fs.String("trace", "", "geri al: bu trace_id'nin acik geri-alma penceresini tetikle")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	*trace = strings.TrimSpace(*trace)
	if *trace == "" {
		fmt.Fprintln(stderr, MsgUndoTraceRequired)
		return 2
	}

	tool, err := client.PolicyUndo(context.Background(), traceid.New(), *trace)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	fmt.Fprintf(stdout, MsgUndoTriggered+"\n", tool)
	return 0
}

// runApprovals implements `kahya approvals` (W3-06): GET /policy/approvals,
// printed one line per pending approval (id, tool, class, summary, age) -
// kahyad/internal/approval.FormatApprovalsList is the SAME formatter the
// task spec calls for, shared with (a future) W3-07 Telegram listing
// rather than a second hand-rolled Printf here.
func runApprovals(client *Client, stdout, stderr io.Writer) int {
	rows, err := client.ListApprovals(context.Background(), traceid.New())
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, MsgApprovalsEmpty)
		return 0
	}
	items := make([]approval.PendingApprovalSummary, len(rows))
	for i, r := range rows {
		items[i] = approval.PendingApprovalSummary{
			ID: r.ID, Tool: r.Tool, Class: r.Class, Summary: r.Summary,
			Age: time.Duration(r.AgeS) * time.Second,
		}
	}
	fmt.Fprint(stdout, approval.FormatApprovalsList(items))
	return 0
}

// runApprove implements `kahya approve <id>` (W3-06): GET /policy/
// approvals?id=<id> for the full byte-exact rendered diff, printed in
// full BEFORE any prompt (WYSIWYE: the human must see reality before
// deciding), then the class-appropriate decision gate - W1/W2's
// [e]vet/[h]ayır, or W3's literal-only "onayla" (HANDOFF §5 safety #5:
// "W3 yazılı 'onayla' YALNIZ yerel yüzeyden kabul edilir" - this CLI IS
// that local surface at W1-W5, so every approve this command sends
// carries surface="local"). A decline calls POST /policy/feedback
// kind="deny" (demoting the ladder, per W3-02), never silently doing
// nothing.
func runApprove(client *Client, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, MsgApproveUsage)
		return 2
	}
	id := strings.TrimSpace(args[0])
	traceID := traceid.New()

	detail, err := client.GetApproval(context.Background(), traceID, id)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	fmt.Fprintln(stdout, detail.Rendered)

	r := bufio.NewReader(stdin)
	var decision approval.Decision
	if detail.Class == "W3" {
		decision, err = approval.PromptLiteral(r, stdout, PromptW3Literal, "onayla")
	} else {
		decision, err = approval.PromptYesNo(r, stdout, PromptW1W2YesNo, "e", "evet")
	}
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}

	if decision != approval.DecisionApprove {
		if _, err := client.PolicyFeedback(context.Background(), traceID, "deny", id, ""); err != nil {
			fmt.Fprintln(stderr, err.Error())
			return 2
		}
		fmt.Fprintln(stdout, MsgApprovalDenied)
		return 1
	}

	if _, err := client.PolicyFeedback(context.Background(), traceID, "approve", id, "local"); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	fmt.Fprintln(stdout, MsgApprovalApproved)
	return 0
}

// runTask implements `kahya task show <id>` and `kahya task resolve <id>
// --retry|--abort` (W4-02).
func runTask(client *Client, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, MsgTaskUsage)
		return 2
	}
	switch args[0] {
	case "show":
		return runTaskShow(client, args[1:], stdout, stderr)
	case "resolve":
		return runTaskResolve(client, args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, MsgTaskUsage)
		return 2
	}
}

// runTaskShow implements `kahya task show <id>`: GET /v1/task/status,
// printed as status/session_id/live-worker-PID/attempts/tool_calls - the
// W4-07 gate script kills the worker via this exact PID.
func runTaskShow(client *Client, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, MsgTaskShowUsage)
		return 2
	}
	id := strings.TrimSpace(args[0])

	ts, err := client.TaskStatus(context.Background(), traceid.New(), id)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}

	session := MsgTaskShowNone
	if ts.SessionID != "" {
		session = ts.SessionID
	}
	pid := MsgTaskShowNone
	if ts.PID != 0 {
		pid = fmt.Sprintf("%d", ts.PID)
	}

	fmt.Fprintf(stdout, MsgTaskShowHeader+"\n", ts.ID)
	fmt.Fprintf(stdout, MsgTaskShowStatus+"\n", ts.Status)
	fmt.Fprintf(stdout, MsgTaskShowSession+"\n", session)
	fmt.Fprintf(stdout, MsgTaskShowPID+"\n", pid)
	fmt.Fprintf(stdout, MsgTaskShowAttempts+"\n", ts.Attempts)
	if len(ts.ToolCalls) == 0 {
		fmt.Fprintln(stdout, MsgTaskShowToolCallsNone)
		return 0
	}
	fmt.Fprintln(stdout, MsgTaskShowToolCallsHead)
	for _, c := range ts.ToolCalls {
		fmt.Fprintf(stdout, MsgTaskShowToolCallRow+"\n", c.Seq, c.Tool, c.Class, c.Status)
	}
	return 0
}

// runTaskResolve implements `kahya task resolve <id> --retry|--abort`
// (W4-02). The task id is a positional argument BEFORE the flag (unlike
// every other flag.FlagSet use in this file), so it is peeled off by hand
// first - flag.Parse stops at the first non-flag argument, which would
// otherwise swallow --retry/--abort as positional args instead of
// recognizing them.
func runTaskResolve(client *Client, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, MsgTaskResolveUsage)
		return 2
	}
	id := strings.TrimSpace(args[0])

	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	retry := fs.Bool("retry", false, "yarıda kesilen aracı yeniden dene (fresh onay ile)")
	abort := fs.Bool("abort", false, "görevi durdur (failed)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *retry == *abort { // neither, or both
		fmt.Fprintln(stderr, MsgTaskResolveUsage)
		return 2
	}

	action := "retry"
	if *abort {
		action = "abort"
	}
	if err := client.TaskResolve(context.Background(), traceid.New(), id, action); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	if *abort {
		fmt.Fprintf(stdout, MsgTaskResolvedAbort+"\n", id)
	} else {
		fmt.Fprintf(stdout, MsgTaskResolvedRetry+"\n", id)
	}
	return 0
}

// runLedger implements `kahya ledger verify` (W4-05) - currently the only
// `kahya ledger` subcommand.
func runLedger(client *Client, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "verify" {
		fmt.Fprintln(stderr, MsgLedgerUsage)
		return 2
	}
	return runLedgerVerify(client, stdout, stderr)
}

// runLedgerVerify implements `kahya ledger verify` (W4-05 task spec step
// 6): POST /v1/ledger/verify, then either print the success line (exit 0)
// or the exact Turkish AlarmMismatch string kahyad returned (exit 1) - a
// mismatch's message is already fully formed server-side, never re-wrapped
// or re-translated here.
func runLedgerVerify(client *Client, stdout, stderr io.Writer) int {
	result, err := client.LedgerVerify(context.Background(), traceid.New())
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	if !result.OK {
		fmt.Fprintln(stderr, result.Message)
		return 1
	}
	fmt.Fprintln(stdout, MsgLedgerVerifyOK)
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
