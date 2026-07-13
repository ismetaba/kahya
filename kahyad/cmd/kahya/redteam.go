package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/eval"
	"kahya/kahyad/internal/traceid"
)

// runEvalRedteam implements `kahya eval redteam` (W78-02): it refuses unless
// KAHYA_ENV=dev, runs the four adversarial scenarios against the isolated
// dev-profile brain.db (in-process, no network, no worker, no cloud),
// prints a Turkish pass/fail table, and exits NON-ZERO if ANY scenario is not
// BLOCKED (a successful bypass). On completion it records the counts/hashes-
// only eval.redteam.result summary row in the PRODUCTION ledger via the
// production kahyad UDS - the ONE production touchpoint (best-effort: a live
// prod daemon is user-assist, so a recording failure is reported but never
// changes the scenario-based exit code).
func runEvalRedteam(stdout, stderr io.Writer) int {
	if os.Getenv("KAHYA_ENV") != config.EnvDev {
		fmt.Fprintln(stderr, MsgEvalRedteamRequiresDev)
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, MsgEvalRedteamRunError+"\n", err.Error())
		return 2
	}
	harness, err := eval.NewHarness(cfg)
	if err != nil {
		fmt.Fprintf(stderr, MsgEvalRedteamRunError+"\n", err.Error())
		return 2
	}

	traceID := traceid.New()
	results, err := harness.Run(context.Background(), traceID)
	if err != nil {
		fmt.Fprintf(stderr, MsgEvalRedteamRunError+"\n", err.Error())
		return 2
	}

	for _, r := range results {
		if r.Blocked {
			fmt.Fprintf(stdout, MsgEvalRedteamBlocked+"\n", r.Name)
		} else {
			fmt.Fprintf(stdout, MsgEvalRedteamBypass+"\n", r.Name)
		}
	}

	sum := eval.Summarize(results, "", traceID)
	fmt.Fprintf(stdout, MsgEvalRedteamSummary+"\n", sum.Blocked, sum.Scenarios, sum.Bypasses)

	// Compute the scenario-set hash and record the summary row in the
	// production ledger (the only production touchpoint), strictly AFTER
	// scenario execution has finished. Both the hash and the record step are
	// best-effort with respect to the exit code: the gate verdict below is
	// decided solely by whether any scenario was bypassed.
	if sha, herr := eval.ComputeScenariosSHA256(config.DefaultRedteamScenariosDir()); herr != nil {
		fmt.Fprintf(stderr, MsgEvalRedteamSummaryNotRecorded+"\n", herr.Error())
	} else if rerr := recordRedteamSummary(traceID, sum, sha); rerr != nil {
		fmt.Fprintf(stderr, MsgEvalRedteamSummaryNotRecorded+"\n", rerr.Error())
	} else {
		fmt.Fprintln(stdout, MsgEvalRedteamSummaryRecorded)
	}

	if sum.Bypasses > 0 {
		fmt.Fprintln(stdout, MsgEvalRedteamRed)
		return 1
	}
	fmt.Fprintln(stdout, MsgEvalRedteamGreen)
	return 0
}

// recordRedteamSummary posts the counts/hashes-only summary to the PRODUCTION
// kahyad over its own socket (resolved independently of the KAHYA_ENV=dev
// profile this command otherwise runs under), so the eval.redteam.result row
// is written by the production daemon - kahyad stays brain.db's sole writer.
func recordRedteamSummary(traceID string, sum eval.RedteamSummary, scenariosSHA256 string) error {
	prodSock, err := config.ProdSocketPath()
	if err != nil {
		return err
	}
	prodClient := newClient(prodSock)
	return prodClient.EvalRedteamRecord(context.Background(), traceID, sum.Scenarios, sum.Blocked, sum.Bypasses, scenariosSHA256)
}
