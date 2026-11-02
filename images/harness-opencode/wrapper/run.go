package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/images/harness-opencode/wrapper/spec"
	"github.com/optimiware/kodama/internal/agents"
)

// Exit codes. Anything the control plane should treat as a failed attempt is a
// failed manifest and exit 0 (the entrypoint reports the command's own
// verdict); the codes below are for the wrapper failing to even say so.
const (
	exitOK        = 0
	exitBootstrap = 2
	exitManifest  = 70
	exitSignalled = 143
)

type options struct {
	args   []string
	getenv func(string) string
	stdout io.Writer
}

// run is the whole wrapper. It writes a provisional failed manifest first and
// replaces it only with a verdict it has reached, so an exit status of zero can
// never complete a step on its own (U-10).
func run(ctx context.Context, o options) int {
	log := slog.New(slog.NewJSONHandler(o.stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	results := o.getenv(spec.EnvResultsFile)
	if results == "" {
		log.Error("no results manifest path", "env", spec.EnvResultsFile)
		return exitBootstrap
	}
	if err := writeManifest(results, manifest{Outcome: "failed", Reason: spec.ReasonIncomplete}); err != nil {
		log.Error("cannot write the provisional manifest", "error", err.Error())
		return exitManifest
	}
	v := execute(ctx, o, log)
	if ctx.Err() != nil && v.signalled {
		// Cancelled: the entrypoint ignores the manifest and the control plane has
		// already revoked the attempt. Leave the provisional failed manifest.
		return exitSignalled
	}
	if err := writeManifest(results, v.manifest); err != nil {
		log.Error("cannot write the manifest", "error", err.Error())
		return exitManifest
	}
	log.Info("wrapper verdict", "outcome", v.manifest.Outcome, "reason", v.manifest.Reason)
	return exitOK
}

type verdict struct {
	manifest  manifest
	signalled bool
}

func failedV(log *slog.Logger, reason, detail string) verdict {
	log.Error("failed", "reason", reason, "detail", detail)
	return verdict{manifest: manifest{Outcome: "failed", Reason: reason}}
}

func execute(ctx context.Context, o options, log *slog.Logger) verdict {
	get := o.getenv
	red := &redactor{}

	// 1. The pinned payload and the compatibility tuple.
	cfg, present, err := agents.DecodeArgs(o.args)
	if err != nil || !present {
		return failedV(log, spec.ReasonPayloadInvalid, "the agent payload is absent or invalid")
	}
	d := cfg.Content
	if d.Wrapper.ID != spec.WrapperID || d.Wrapper.Version != spec.WrapperVersion ||
		d.Wrapper.Harness.ID != spec.HarnessID || d.Wrapper.Harness.Version != spec.HarnessVersion {
		return failedV(log, spec.ReasonVersionMismatch, "the pinned wrapper or harness is not the one this image carries")
	}
	attempt, workspace, outputs := get(spec.EnvAttemptID), get(spec.EnvWorkspace), get(spec.EnvOutputsDir)
	platform := get(spec.EnvPlatformURL)
	if platform == "" {
		platform = parentServerURL()
	}
	tokenFile := orDefault(get(spec.EnvTokenFile), spec.DefaultTokenFile)
	caFile := orDefault(get(spec.EnvCAFile), spec.DefaultCAFile)
	bin := orDefault(get(spec.EnvOpenCodeBin), spec.OpenCodePath)
	if attempt == "" || workspace == "" || outputs == "" || platform == "" {
		return failedV(log, spec.ReasonConfigInvalid, "attempt, workspace, outputs or platform URL is not set")
	}
	idle := 15 * time.Minute
	if s := get(spec.EnvIdleSeconds); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return failedV(log, spec.ReasonConfigInvalid, "the idle bound is not a positive integer")
		}
		idle = time.Duration(n) * time.Second
	}
	outTokens := int64(4096)
	if s := get(spec.EnvOutputTokens); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 1 {
			return failedV(log, spec.ReasonConfigInvalid, "the output token cap is not a positive integer")
		}
		outTokens = n
	}
	srv, err := servers(d.Tools)
	if err != nil {
		return failedV(log, spec.ReasonConfigInvalid, err.Error())
	}
	if tok, err := os.ReadFile(tokenFile); err == nil {
		red.Add(string(tok))
	}

	// 2. The pinned context: the baseline carried by the payload must be the one
	// the control plane pinned for this attempt (AR-14), and the outputs the step
	// declared decide what the wrapper may write.
	rctx, err := fetchContext(ctx, platform, attempt, tokenFile, caFile)
	if err != nil {
		return failedV(log, spec.ReasonContextMismatch, "the pinned context could not be read: "+red.String(err.Error()))
	}
	prof, base := cfg.Pins()
	if rctx.AttemptID != attempt || rctx.DesignBaseline == nil || rctx.DesignBaseline.Digest != base.Digest ||
		rctx.Profile == nil || *rctx.Profile != prof {
		return failedV(log, spec.ReasonContextMismatch, "the payload does not match the pinned attempt context")
	}
	declared := map[string]runtimev1.OutputDecl{}
	for _, od := range rctx.Outputs {
		declared[od.Name] = od
	}
	if od, ok := declared[spec.ConformanceOutput]; !ok || od.Type != runtimev1.OutputArtifact || od.Path == nil || *od.Path != spec.DeclaredOutputPath(spec.ConformanceFile) {
		return failedV(log, spec.ReasonOutputUndeclared, "the step does not declare the conformance artifact output")
	}
	if d.Transcript.Record {
		if od, ok := declared[spec.TranscriptOutput]; !ok || od.Type != runtimev1.OutputArtifact || od.Path == nil || *od.Path != spec.DeclaredOutputPath(spec.TranscriptFile) {
			return failedV(log, spec.ReasonOutputUndeclared, "the profile records transcripts but the step does not declare the transcript output")
		}
	}

	// 2b. A source-set attempt (runtime context v3) works in one checkout per source.
	// The plan says where each checkout's changes are committed; it must be one the
	// attempt's own Git grant lets the entrypoint publish, or nothing is run. The
	// commit each checkout starts on is read from the checkout itself.
	var (
		plan   []agents.Checkout
		starts map[string]runtimev1.GitCommit
	)
	if rctx.V3 != nil {
		if plan, err = agents.PlanCheckouts(*rctx.V3); err != nil {
			return failedV(log, spec.ReasonSourceInvalid, err.Error())
		}
		if starts, err = startCommits(ctx, workspace, plan); err != nil {
			return failedV(log, spec.ReasonSourceInvalid, err.Error())
		}
	}

	// 3. The private OpenCode home and configuration.
	scratch, err := os.MkdirTemp(orDefault(get("TMPDIR"), os.TempDir()), "kodama-opencode-")
	if err != nil {
		return failedV(log, spec.ReasonConfigInvalid, "no private scratch directory: "+err.Error())
	}
	defer os.RemoveAll(scratch)
	home := filepath.Join(scratch, "home")
	xdg := map[string]string{"XDG_CONFIG_HOME": filepath.Join(home, ".config"), "XDG_DATA_HOME": filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME": filepath.Join(home, ".local", "state"), "XDG_CACHE_HOME": filepath.Join(home, ".cache")}
	for _, dir := range append([]string{filepath.Join(xdg["XDG_CONFIG_HOME"], "opencode")}, xdg["XDG_DATA_HOME"], xdg["XDG_STATE_HOME"], xdg["XDG_CACHE_HOME"]) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return failedV(log, spec.ReasonConfigInvalid, err.Error())
		}
	}
	ocfg, err := openCodeConfig(cfg, srv, outTokens)
	if err != nil {
		return failedV(log, spec.ReasonConfigInvalid, err.Error())
	}
	if err := os.WriteFile(filepath.Join(xdg["XDG_CONFIG_HOME"], "opencode", "opencode.json"), ocfg, 0o600); err != nil {
		return failedV(log, spec.ReasonConfigInvalid, err.Error())
	}
	env := childEnv(get, home, xdg)

	// 4. The pinned harness version, measured, not assumed.
	vcmd := exec.CommandContext(ctx, bin, "--version")
	vcmd.Env, vcmd.Dir = env, workspace
	if out, err := vcmd.Output(); err != nil || !strings.Contains(string(out), spec.HarnessVersion) {
		return failedV(log, spec.ReasonVersionMismatch, "opencode --version does not report "+spec.HarnessVersion)
	}

	// 5. Containment and credential delivery.
	if err := becomeSubreaper(); err != nil {
		return failedV(log, spec.ReasonConfigInvalid, "cannot become a subreaper: "+err.Error())
	}
	regs := make([]string, 0, len(srv))
	for _, s := range srv {
		regs = append(regs, s.Registration)
	}
	px, err := newProxy(platform, attempt, tokenFile, caFile, regs, red.Add)
	if err != nil {
		return failedV(log, spec.ReasonConfigInvalid, err.Error())
	}
	if err := px.start(); err != nil {
		return failedV(log, spec.ReasonConfigInvalid, err.Error())
	}
	defer px.stop()

	// 6. Run the harness. Its stdout is the NDJSON event stream; it is never
	// copied to this process's stdout (the retained command log).
	cmd := exec.Command(bin, "run", "--standalone", "--format", "json", "--model", spec.ProviderID+"/"+d.Model.Name,
		"--title", "kodama attempt", prompt(cfg, rctx))
	cmd.Dir, cmd.Env = workspace, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The pipes are owned here, not by exec.Cmd, so Wait can never close them
	// under a reader that has not seen the harness's last line.
	outR, outW, err := os.Pipe()
	if err != nil {
		return failedV(log, spec.ReasonStartFailed, err.Error())
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return failedV(log, spec.ReasonStartFailed, err.Error())
	}
	defer outR.Close()
	defer errR.Close()
	cmd.Stdout, cmd.Stderr = outW, errW
	// Stdin is /dev/null (exec.Cmd's default for a nil Stdin): there is no human
	// to answer anything.
	startErr := cmd.Start()
	outW.Close()
	errW.Close()
	if startErr != nil {
		return failedV(log, spec.ReasonStartFailed, "cannot start the harness")
	}
	stdout, stderr := io.Reader(outR), io.Reader(errR)
	log.Info("harness started", "version", spec.HarnessVersion, "pid", cmd.Process.Pid, "recording", d.Transcript.Record)

	var tr *os.File
	if d.Transcript.Record {
		tr, err = os.OpenFile(filepath.Join(outputs, spec.TranscriptFile), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			killTree(os.Getpid(), time.Second, false)
			return failedV(log, spec.ReasonOutputUndeclared, "the transcript file cannot be created")
		}
		defer tr.Close()
	}
	sum := newSummary()
	activity := make(chan struct{}, 1)
	pumped := make(chan struct{}, 2)
	go func() {
		defer func() { pumped <- struct{}{} }()
		_ = pump(stdout, func(line []byte) {
			sum.observe(line)
			if tr != nil {
				_, _ = tr.WriteString(red.String(string(line)) + "\n")
			}
			select {
			case activity <- struct{}{}:
			default:
			}
		})
	}()
	go func() {
		defer func() { pumped <- struct{}{} }()
		_ = pump(stderr, func(line []byte) {
			sum.observeStderr(string(line))
			if tr != nil {
				_, _ = tr.WriteString(`{"type":"stderr","text":` + jsonString(red.String(string(line))) + "}\n")
			}
			select {
			case activity <- struct{}{}:
			default:
			}
		})
	}()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	var (
		exitErr  error
		idled    bool
		signal   bool
		finished bool
	)
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for !finished {
		select {
		case <-activity:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)
		case <-timer.C:
			idled = true
			finished = true
			killTree(os.Getpid(), 3*time.Second, false)
			exitErr = <-waited
		case <-ctx.Done():
			signal, finished = true, true
			killTree(os.Getpid(), 3*time.Second, false)
			exitErr = <-waited
		case exitErr = <-waited:
			finished = true
		}
	}
	// A background process the agent left behind belongs to this attempt: the
	// whole tree ends with the harness.
	n, empty := killTree(os.Getpid(), 3*time.Second, true)
	for i := 0; i < 2; i++ {
		select {
		case <-pumped:
		case <-time.After(2 * time.Second):
		}
	}
	log.Info("harness ended", "swept", n, "treeEmpty", empty, "lines", sum.Lines, "types", sum.Types, "tools", sum.Tools,
		"toolErrors", sum.ToolErrors, "stepsFinished", sum.StepsFinished, "autoRejected", sum.AutoRejected, "proxy", px.Requests())

	if d.Transcript.Record {
		for _, m := range sum.ErrorMessages {
			log.Warn("harness error text (recording authorized)", "text", red.String(m))
		}
	}
	switch {
	case signal:
		return verdict{signalled: true}
	case idled:
		return failedV(log, spec.ReasonIdle, "no harness output within the idle bound")
	case !empty:
		return failedV(log, spec.ReasonHarnessError, "descendants survived the sweep")
	case len(sum.Errors) > 0:
		// An error event explains a non-zero exit, so it is the more specific reason.
		return failedV(log, spec.ReasonHarnessError, "the harness reported "+strconv.Itoa(len(sum.Errors))+" error event(s): "+strings.Join(sum.Errors, ","))
	case exitErr != nil:
		var ee *exec.ExitError
		if errors.As(exitErr, &ee) {
			return failedV(log, spec.ReasonExitNonzero, "the harness exited with status "+strconv.Itoa(ee.ExitCode()))
		}
		return failedV(log, spec.ReasonHarnessError, "waiting for the harness failed")
	}

	// 7. Structured completion, fail closed.
	res, state, rerr := readAgentResult(workspace)
	if rerr != nil {
		return failedV(log, state, rerr.Error())
	}
	switch res.Outcome {
	case "needs_input":
		return verdict{manifest: manifest{Outcome: "needs_input", Question: strings.TrimSpace(red.String(res.Question))}}
	case "failed":
		reason := res.Reason
		if reason == "" {
			reason = spec.ReasonAgentReportedFail
		}
		return verdict{manifest: manifest{Outcome: "failed", Reason: reason}}
	}

	// completed: commit what the agent changed, then bind the conformance report
	// to that candidate and to the pinned baseline.
	var candidate string
	if plan != nil {
		// The candidate of a source-set attempt is the commit of every checkout, not
		// one commit. The conformance report names the primary source's commit; the
		// whole set is identified by its digest and published by the entrypoint.
		set, cerr := commitSources(ctx, workspace, plan, starts, res.Summary, red.String)
		if cerr != nil {
			var se *sourceError
			if errors.As(cerr, &se) {
				// A checkout the agent left in a state the wrapper will not run Git in.
				return failedV(log, spec.ReasonSourceInvalid, cerr.Error())
			}
			return failedV(log, spec.ReasonCommitFailed, cerr.Error())
		}
		digest, _ := set.Digest()
		candidate = string(set.Primary().Commit)
		log.Info("candidate committed", "sources", len(set.Sources), "changed", len(set.Changed()), "digest", digest)
	} else {
		var cerr error
		if candidate, cerr = commitWorkspace(ctx, workspace, res.Summary, red.String); cerr != nil {
			return failedV(log, spec.ReasonCommitFailed, cerr.Error())
		}
	}
	rep, notes, berr := buildReport(*rctx.DesignBaseline, candidate, res.Conformance, red.String)
	if berr != nil {
		return failedV(log, spec.ReasonResultInvalid, "the conformance claims are not a valid report: "+berr.Error())
	}
	for _, n := range notes {
		log.Warn("conformance note", "note", n)
	}
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return failedV(log, spec.ReasonResultInvalid, err.Error())
	}
	if err := os.WriteFile(filepath.Join(outputs, spec.ConformanceFile), append(raw, '\n'), 0o600); err != nil {
		return failedV(log, spec.ReasonOutputUndeclared, "the conformance output cannot be written")
	}
	log.Info("conformance report written", "disposition", rep.Disposition, "deviations", len(rep.UnresolvedDeviations), "candidate", candidate)
	return verdict{manifest: manifest{Outcome: "completed"}}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// childEnv is the harness environment, built explicitly: nothing is inherited
// except what a command needs to find tools. No credential is in it.
func childEnv(get func(string) string, home string, xdg map[string]string) []string {
	env := []string{
		"PATH=" + orDefault(get("PATH"), "/usr/local/bin:/usr/bin:/bin"),
		"HOME=" + home,
		"TMPDIR=" + orDefault(get("TMPDIR"), os.TempDir()),
		"LANG=" + orDefault(get("LANG"), "C.UTF-8"),
		spec.ModelKeyEnv + "=" + spec.ModelKeyValue,
		// Measured against 2.1.2: the workspace opencode.json overrides the user
		// provider endpoint; this variable ignores project configuration. The
		// managed configuration remains the documented backstop. These names are
		// present in the binary and absent from the V2 documentation.
		"OPENCODE_DISABLE_PROJECT_CONFIG=1",
		"OPENCODE_DISABLE_MODELS_FETCH=1",
		"OPENCODE_DISABLE_AUTOUPDATE=1",
		"OPENCODE_DISABLE_CLAUDE_CODE=1",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		spec.EnvAttemptID + "=" + get(spec.EnvAttemptID),
		spec.EnvWorkspace + "=" + get(spec.EnvWorkspace),
	}
	for k, v := range xdg {
		env = append(env, k+"="+v)
	}
	return env
}

// prompt is the instruction context, fully derived from pinned inputs: the
// profile's instructions, the baseline's references (never resolved again), the
// pinned work item and the completion protocol.
func prompt(cfg agents.WrapperConfig, c pinnedContext) string {
	var b bytes.Buffer
	b.WriteString(strings.TrimSpace(cfg.Content.Instructions))
	b.WriteString("\n\n## Pinned design baseline\n")
	b.WriteString("These are references at immutable revisions. Do not resolve a branch, tag or `latest` for them, and do not treat them as permission to use any tool.\n")
	bl := cfg.Content.DesignBaseline
	b.WriteString("Baseline digest: " + string(bl.Digest) + "\n")
	for _, r := range bl.Requirements {
		b.WriteString("- requirement " + r + "\n")
	}
	for _, r := range bl.Decisions {
		b.WriteString("- decision " + r + "\n")
	}
	for _, s := range bl.Sources {
		b.WriteString("- source " + s.Repository + " @ " + string(s.Revision) + " : " + s.Path + "\n")
	}
	for _, r := range bl.RequiredChecks {
		b.WriteString("- required check " + r + "\n")
	}
	if c.WorkItem != nil {
		b.WriteString("\n## Work item " + string(c.WorkItem.ID) + "\n" + strings.TrimSpace(c.WorkItem.Spec) + "\n")
		for _, ac := range c.WorkItem.AcceptanceCriteria {
			b.WriteString("- " + ac.ID + ": " + ac.Text + "\n")
		}
	}
	if c.V3 != nil {
		if plan, err := agents.PlanCheckouts(*c.V3); err == nil {
			b.WriteString("\n## Repositories\n")
			b.WriteString("Each repository is checked out under the workspace at the path shown (`.` is the workspace itself). " +
				"Edit files in place. Do not run `git push`, `git fetch`, `git remote`, `git config` or any credential command, " +
				"and do not switch branches: you hold no Git credential, and the platform commits and publishes your changes after you finish.\n")
			for _, co := range plan {
				where := "published to a run-scoped agent branch"
				if co.Mode == agents.ModeHead {
					where = "published to the work item's task branch"
				}
				b.WriteString("- `" + co.Path + "` (repository " + co.RepositoryID + ", " + string(co.Access) + " access): changes are " + where + "\n")
			}
		}
	}
	b.WriteString("\n## Completion protocol (this run has no human)\n")
	b.WriteString("Nobody can answer questions or approve prompts. Do not ask interactively. " +
		"When you are done, write the JSON file `" + spec.ResultFile + "` in the workspace root: " +
		`{"outcome":"completed"|"needs_input"|"failed","summary":"…","question":"…","reason":"…","conformance":{"requirements":[…],"checks":[{"id":"…","outcome":"passed|failed|not_run","evidence":"…"}],"deviations":[{"requirement":"…","reason":"…"}]}}. ` +
		"Use needs_input with a question when business input is missing, and report any material conflict with the baseline under conformance.deviations instead of inventing a different design. " +
		"A prose reply is not a result: without a valid result file the step fails.\n")
	return b.String()
}

// parentServerURL recovers the platform origin from the step entrypoint that
// launched the wrapper: `kodama step --server-url=<origin> ...` is the
// entrypoint's own documented flag and the wrapper is its direct child. The
// entrypoint does not export the origin to its command's environment and an
// agent step cannot carry environment of its own, so this is the only
// installation-independent source today (a recorded prerequisite: the
// entrypoint should export KODAMA_SERVER_URL). It is accepted only when the
// parent really is `kodama step`.
func parentServerURL() string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(os.Getppid()) + "/cmdline")
	if err != nil {
		return ""
	}
	argv := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
	if len(argv) < 2 || filepath.Base(argv[0]) != "kodama" || argv[1] != "step" {
		return ""
	}
	for i, a := range argv[2:] {
		if v, ok := strings.CutPrefix(a, "--server-url="); ok {
			return v
		}
		if a == "--server-url" && i+3 < len(argv) {
			return argv[i+3]
		}
	}
	return ""
}
