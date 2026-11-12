// Command testsuite plans and runs one target of the unified test suite. See
// docs/runbooks/unified-test-suite.md; scripts/test-suite is the entry point
// that also prepares a stage session.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/optimiware/kodama/test/suite"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("testsuite", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var (
		target       = flags.String("target", "local-all", "what to run: podman, kind, stage or local-all")
		installation = flags.String("installation", "", "installation descriptor of an existing installation (default $Q01_INSTALLATION_FILE)")
		evidenceDir  = flags.String("evidence-dir", "", "absolute directory for evidence, lane logs and summary.json (default /tmp/oc/evidence/suite-<target>-<UTC time>)")
		concurrency  = flags.Int("concurrency", 0, "scenario concurrency of every lane (0 keeps each lane's default, 1 runs scenarios one at a time)")
		keepGoing    = flags.Bool("keep-going", false, "start the remaining waves after a failed one")
		skipChaos    = flags.Bool("skip-chaos", false, "stage: leave out the disruptive second wave")
		skipQ03      = flags.Bool("skip-q03", false, "leave out the restore drills")
		q03Mode      = flags.String("q03-mode", "plan", "stage: RDB drill mode: plan, restore or drop")
		race         = flags.Bool("race", true, "run go test with the race detector")
		tokenRefresh = flags.Duration("token-refresh", 0, "stage: refresh the persona token files this often (0 never; the stage session uses 5m)")
		dryRun       = flags.Bool("dry-run", false, "print the plan and run nothing")
	)
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "testsuite: unexpected arguments: %v\n", flags.Args())
		return 2
	}
	fail := func(code int, err error) int {
		fmt.Fprintf(stderr, "testsuite: %v\n", err)
		return code
	}

	options := suite.Options{
		Target:       suite.Target(*target),
		Installation: *installation,
		EvidenceDir:  *evidenceDir,
		Concurrency:  *concurrency,
		Race:         *race,
		SkipChaos:    *skipChaos,
		SkipQ03:      *skipQ03,
		Q03Mode:      *q03Mode,
	}
	if options.Installation == "" {
		options.Installation = strings.TrimSpace(os.Getenv("Q01_INSTALLATION_FILE"))
	}
	if options.EvidenceDir == "" {
		options.EvidenceDir = filepath.Join("/tmp/oc/evidence",
			fmt.Sprintf("suite-%s-%s", options.Target, time.Now().UTC().Format("20060102T150405Z")))
	}

	if *dryRun {
		plan, err := suite.BuildPlan(options, os.Getenv)
		if err != nil {
			return fail(2, err)
		}
		printPlan(stdout, plan)
		return 0
	}

	if _, err := os.Stat("scripts/infra-slot.sh"); err != nil {
		return fail(2, fmt.Errorf("run testsuite from the repository root: %w", err))
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	var refresh func()
	if options.Target == suite.TargetStage && options.Installation != "" {
		tokens, err := suite.OpenStageTokens(ctx, options.Installation, os.Getenv)
		if err != nil {
			return fail(2, err)
		}
		defer tokens.Close()
		options.SecondAdminTokenFile = tokens.SecondAdminFile
		refresher, ok := tokens.Refresher()
		switch {
		case *tokenRefresh <= 0:
		case !ok:
			fmt.Fprintln(stderr, "testsuite: token refresh is off: the run owns no persona token file, or the descriptor names no OAuthSonas access code file")
		default:
			refresh = func() {
				refresher.Run(ctx, *tokenRefresh, func(err error) {
					fmt.Fprintf(stderr, "testsuite: token refresh failed; the previous tokens stay in place: %v\n", err)
				})
			}
		}
	}

	plan, err := suite.BuildPlan(options, os.Getenv)
	if err != nil {
		return fail(2, err)
	}
	if err := os.MkdirAll(options.EvidenceDir, 0o700); err != nil {
		return fail(1, fmt.Errorf("create the evidence directory: %w", err))
	}
	fmt.Fprintf(stdout, "suite: evidence directory %s\n", options.EvidenceDir)
	if refresh != nil {
		go refresh()
	}

	report, runErr := suite.RunPlan(ctx, plan, suite.OSExecutor{}, suite.RunOptions{
		KeepGoing: *keepGoing,
		Output:    stdout,
		LogDir:    filepath.Join(options.EvidenceDir, "logs"),
	})
	printSummary(stdout, report)
	summary := filepath.Join(options.EvidenceDir, "summary.json")
	if err := suite.WriteReport(summary, report); err != nil {
		return fail(1, err)
	}
	fmt.Fprintf(stdout, "suite: summary %s\n", summary)
	if runErr != nil {
		return fail(1, runErr)
	}
	return 0
}

func printPlan(out io.Writer, plan suite.Plan) {
	fmt.Fprintf(out, "target %s: %d wave(s)\n", plan.Target, len(plan.Waves))
	for _, wave := range plan.Waves {
		fmt.Fprintf(out, "%s: %d concurrent lane(s)\n", wave.Name, len(wave.Lanes))
		for _, lane := range wave.Lanes {
			fmt.Fprintf(out, "  %s\n    %s\n", lane.Name, strings.Join(lane.Argv, " "))
			for _, entry := range lane.Env {
				fmt.Fprintf(out, "    %s\n", entry)
			}
		}
	}
}

func printSummary(out io.Writer, report suite.Report) {
	verdict := "PASS"
	if !report.Passed {
		verdict = "FAIL"
	}
	fmt.Fprintf(out, "suite: %s %s in %s\n", report.Target, verdict, milliseconds(report.DurationMS))
	for _, wave := range report.Waves {
		for _, lane := range wave.Lanes {
			result := "PASS"
			switch {
			case lane.Error != "":
				result = "FAIL (" + lane.Error + ")"
			case !lane.Passed:
				result = fmt.Sprintf("FAIL (exit %d)", lane.ExitCode)
			}
			fmt.Fprintf(out, "  %-18s %-36s %-16s %s\n", wave.Name, lane.Name, result, milliseconds(lane.DurationMS))
		}
	}
	for _, wave := range report.SkippedWaves {
		fmt.Fprintf(out, "  %-18s not started: an earlier wave failed\n", wave)
	}
}

func milliseconds(value int64) time.Duration {
	return (time.Duration(value) * time.Millisecond).Round(100 * time.Millisecond)
}
